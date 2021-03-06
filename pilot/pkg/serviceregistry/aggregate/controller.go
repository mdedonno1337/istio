// Copyright 2017 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package aggregate

import (
	"sync"

	multierror "github.com/hashicorp/go-multierror"

	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/serviceregistry"
	"istio.io/istio/pkg/log"
)

// Registry specifies the collection of service registry related interfaces
type Registry struct {
	Name      serviceregistry.ServiceRegistry
	ClusterID string
	model.Controller
	model.ServiceDiscovery
	model.ServiceAccounts
}

var (
	clusterAddressesMutex sync.Mutex
)

// Controller aggregates data across different registries and monitors for changes
type Controller struct {
	registries []Registry
	storeLock  sync.RWMutex
}

// NewController creates a new Aggregate controller
func NewController() *Controller {

	return &Controller{
		registries: []Registry{},
	}
}

// AddRegistry adds registries into the aggregated controller
func (c *Controller) AddRegistry(registry Registry) {
	c.storeLock.Lock()
	defer c.storeLock.Unlock()

	registries := c.registries
	registries = append(registries, registry)
	c.registries = registries
}

// DeleteRegistry deletes specified registry from the aggregated controller
func (c *Controller) DeleteRegistry(clusterID string) {
	c.storeLock.Lock()
	defer c.storeLock.Unlock()

	if len(c.registries) == 0 {
		log.Warnf("Registry list is empty, nothing to delete")
		return
	}
	index, ok := c.GetRegistryIndex(clusterID)
	if !ok {
		log.Warnf("Registry is not found in the registries list, nothing to delete")
		return
	}
	registries := c.registries
	registries = append(registries[:index], registries[index+1:]...)
	c.registries = registries
	log.Infof("Registry for the cluster %s has been deleted.", clusterID)
}

// GetRegistries returns a copy of all registries
func (c *Controller) GetRegistries() []Registry {
	c.storeLock.Lock()
	defer c.storeLock.Unlock()

	return c.registries
}

// GetRegistryIndex returns the index of a registry
func (c *Controller) GetRegistryIndex(clusterID string) (int, bool) {
	for i, r := range c.registries {
		if r.ClusterID == clusterID {
			return i, true
		}
	}
	return 0, false
}

// Services lists services from all platforms
func (c *Controller) Services() ([]*model.Service, error) {
	// smap is a map of hostname (string) to service, used to identify services that
	// are installed in multiple clusters.
	smap := make(map[model.Hostname]*model.Service)

	services := make([]*model.Service, 0)
	var errs error
	// Locking Registries list while walking it to prevent inconsistent results
	for _, r := range c.GetRegistries() {
		svcs, err := r.Services()
		if err != nil {
			errs = multierror.Append(errs, err)
			continue
		}
		// Race condition: multiple threads may call Services, and multiple services
		// may modify one of the service's cluster ID
		clusterAddressesMutex.Lock()
		for _, s := range svcs {
			sp, ok := smap[s.Hostname]
			if !ok {
				// First time we see a service. The result will have a single service per hostname
				// The first cluster will be listed first, so the services in the primary cluster
				// will be used for default settings. If a service appears in multiple clusters,
				// the order is less clear.
				sp = s
				smap[s.Hostname] = sp
				services = append(services, sp)
			}

			// If the registry has a cluster ID, keep track of the cluster and the
			// local address inside the cluster.
			// TODO: what is this used for ? Do we want to support multiple VIPs, or
			// only use the 'primary' VIP ?
			if r.ClusterID != "" {
				if sp.Addresses == nil {
					sp.Addresses = make(map[string]string)
				}
				sp.Addresses[r.ClusterID] = s.Address
				smap[s.Hostname] = sp
			}
		}
		clusterAddressesMutex.Unlock()
	}
	return services, errs
}

// GetService retrieves a service by hostname if exists
func (c *Controller) GetService(hostname model.Hostname) (*model.Service, error) {
	var errs error
	for _, r := range c.GetRegistries() {
		service, err := r.GetService(hostname)
		if err != nil {
			errs = multierror.Append(errs, err)
		} else if service != nil {
			if errs != nil {
				log.Warnf("GetService() found match but encountered an error: %v", errs)
			}
			return service, nil
		}

	}
	return nil, errs
}

// ManagementPorts retrieves set of health check ports by instance IP
// Return on the first hit.
func (c *Controller) ManagementPorts(addr string) model.PortList {
	for _, r := range c.GetRegistries() {
		if portList := r.ManagementPorts(addr); portList != nil {
			return portList
		}
	}
	return nil
}

// Instances retrieves instances for a service and its ports that match
// any of the supplied labels. All instances match an empty label list.
func (c *Controller) Instances(hostname model.Hostname, ports []string,
	labels model.LabelsCollection) ([]*model.ServiceInstance, error) {
	var instances, tmpInstances []*model.ServiceInstance
	var errs error
	for _, r := range c.GetRegistries() {
		var err error
		tmpInstances, err = r.Instances(hostname, ports, labels)
		if err != nil {
			errs = multierror.Append(errs, err)
		} else if len(tmpInstances) > 0 {
			if errs != nil {
				log.Warnf("Instances() found match but encountered an error: %v", errs)
			}
			instances = append(instances, tmpInstances...)
		}
	}
	if len(instances) > 0 {
		errs = nil
	}
	return instances, errs
}

// InstancesByPort retrieves instances for a service on a given port that match
// any of the supplied labels. All instances match an empty label list.
func (c *Controller) InstancesByPort(hostname model.Hostname, port int,
	labels model.LabelsCollection) ([]*model.ServiceInstance, error) {
	var instances, tmpInstances []*model.ServiceInstance
	var errs error
	for _, r := range c.GetRegistries() {
		var err error
		tmpInstances, err = r.InstancesByPort(hostname, port, labels)
		if err != nil {
			errs = multierror.Append(errs, err)
		} else if len(tmpInstances) > 0 {
			if errs != nil {
				log.Warnf("Instances() found match but encountered an error: %v", errs)
			}
			instances = append(instances, tmpInstances...)
		}
	}
	if len(instances) > 0 {
		errs = nil
	}
	return instances, errs
}

// GetProxyServiceInstances lists service instances co-located with a given proxy
func (c *Controller) GetProxyServiceInstances(node *model.Proxy) ([]*model.ServiceInstance, error) {
	out := make([]*model.ServiceInstance, 0)
	var errs error
	// It doesn't make sense for a single proxy to be found in more than one registry.
	// TODO: if otherwise, warning or else what to do about it.
	for _, r := range c.GetRegistries() {
		instances, err := r.GetProxyServiceInstances(node)
		if err != nil {
			errs = multierror.Append(errs, err)
		} else if len(instances) > 0 {
			out = append(out, instances...)
			node.ClusterID = r.ClusterID
			break
		}
	}

	if len(out) > 0 {
		if errs != nil {
			log.Warnf("GetProxyServiceInstances() found match but encountered an error: %v", errs)
		}
		return out, nil
	}

	return out, errs
}

// Run starts all the controllers
func (c *Controller) Run(stop <-chan struct{}) {

	for _, r := range c.GetRegistries() {
		go r.Run(stop)
	}

	<-stop
	log.Info("Registry Aggregator terminated")
}

// AppendServiceHandler implements a service catalog operation
func (c *Controller) AppendServiceHandler(f func(*model.Service, model.Event)) error {
	for _, r := range c.GetRegistries() {
		if err := r.AppendServiceHandler(f); err != nil {
			log.Infof("Fail to append service handler to adapter %s", r.Name)
			return err
		}
	}
	return nil
}

// AppendInstanceHandler implements a service instance catalog operation
func (c *Controller) AppendInstanceHandler(f func(*model.ServiceInstance, model.Event)) error {
	for _, r := range c.GetRegistries() {
		if err := r.AppendInstanceHandler(f); err != nil {
			log.Infof("Fail to append instance handler to adapter %s", r.Name)
			return err
		}
	}
	return nil
}

// GetIstioServiceAccounts implements model.ServiceAccounts operation
func (c *Controller) GetIstioServiceAccounts(hostname model.Hostname, ports []string) []string {
	for _, r := range c.GetRegistries() {
		if svcAccounts := r.GetIstioServiceAccounts(hostname, ports); svcAccounts != nil {
			return svcAccounts
		}
	}
	return nil
}
