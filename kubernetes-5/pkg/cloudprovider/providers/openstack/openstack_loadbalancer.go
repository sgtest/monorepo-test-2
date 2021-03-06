/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package openstack

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/lbaas/members"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/lbaas/monitors"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/lbaas/pools"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/lbaas/vips"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/lbaas_v2/listeners"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/lbaas_v2/loadbalancers"
	v2monitors "github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/lbaas_v2/monitors"
	v2pools "github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/lbaas_v2/pools"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/security/groups"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/security/rules"
	neutronports "github.com/gophercloud/gophercloud/openstack/networking/v2/ports"
	"github.com/gophercloud/gophercloud/pagination"

	"github.com/sourcegraph/monorepo-test-1/kubernetes-5/pkg/api/v1"
	"github.com/sourcegraph/monorepo-test-1/kubernetes-5/pkg/api/v1/service"
	"github.com/sourcegraph/monorepo-test-1/kubernetes-5/pkg/cloudprovider"
)

// Note: when creating a new Loadbalancer (VM), it can take some time before it is ready for use,
// this timeout is used for waiting until the Loadbalancer provisioning status goes to ACTIVE state.
const loadbalancerActiveTimeoutSeconds = 120
const loadbalancerDeleteTimeoutSeconds = 30

// LoadBalancer implementation for LBaaS v1
type LbaasV1 struct {
	LoadBalancer
}

// LoadBalancer implementation for LBaaS v2
type LbaasV2 struct {
	LoadBalancer
}

type empty struct{}

func networkExtensions(client *gophercloud.ServiceClient) (map[string]bool, error) {
	seen := make(map[string]bool)

	pager := extensions.List(client)
	err := pager.EachPage(func(page pagination.Page) (bool, error) {
		exts, err := extensions.ExtractExtensions(page)
		if err != nil {
			return false, err
		}
		for _, ext := range exts {
			seen[ext.Alias] = true
		}
		return true, nil
	})

	return seen, err
}

func getPortByIP(client *gophercloud.ServiceClient, ipAddress string) (neutronports.Port, error) {
	var targetPort neutronports.Port
	var portFound = false

	err := neutronports.List(client, neutronports.ListOpts{}).EachPage(func(page pagination.Page) (bool, error) {
		portList, err := neutronports.ExtractPorts(page)
		if err != nil {
			return false, err
		}

		for _, port := range portList {
			for _, ip := range port.FixedIPs {
				if ip.IPAddress == ipAddress {
					targetPort = port
					portFound = true
					return false, nil
				}
			}
		}

		return true, nil
	})
	if err == nil && !portFound {
		err = ErrNotFound
	}
	return targetPort, err
}

func getFloatingIPByPortID(client *gophercloud.ServiceClient, portID string) (*floatingips.FloatingIP, error) {
	opts := floatingips.ListOpts{
		PortID: portID,
	}
	pager := floatingips.List(client, opts)

	floatingIPList := make([]floatingips.FloatingIP, 0, 1)

	err := pager.EachPage(func(page pagination.Page) (bool, error) {
		f, err := floatingips.ExtractFloatingIPs(page)
		if err != nil {
			return false, err
		}
		floatingIPList = append(floatingIPList, f...)
		if len(floatingIPList) > 1 {
			return false, ErrMultipleResults
		}
		return true, nil
	})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	if len(floatingIPList) == 0 {
		return nil, ErrNotFound
	} else if len(floatingIPList) > 1 {
		return nil, ErrMultipleResults
	}

	return &floatingIPList[0], nil
}

func getPoolByName(client *gophercloud.ServiceClient, name string) (*pools.Pool, error) {
	opts := pools.ListOpts{
		Name: name,
	}
	pager := pools.List(client, opts)

	poolList := make([]pools.Pool, 0, 1)

	err := pager.EachPage(func(page pagination.Page) (bool, error) {
		p, err := pools.ExtractPools(page)
		if err != nil {
			return false, err
		}
		poolList = append(poolList, p...)
		if len(poolList) > 1 {
			return false, ErrMultipleResults
		}
		return true, nil
	})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	if len(poolList) == 0 {
		return nil, ErrNotFound
	} else if len(poolList) > 1 {
		return nil, ErrMultipleResults
	}

	return &poolList[0], nil
}

func getVipByName(client *gophercloud.ServiceClient, name string) (*vips.VirtualIP, error) {
	opts := vips.ListOpts{
		Name: name,
	}
	pager := vips.List(client, opts)

	vipList := make([]vips.VirtualIP, 0, 1)

	err := pager.EachPage(func(page pagination.Page) (bool, error) {
		v, err := vips.ExtractVIPs(page)
		if err != nil {
			return false, err
		}
		vipList = append(vipList, v...)
		if len(vipList) > 1 {
			return false, ErrMultipleResults
		}
		return true, nil
	})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	if len(vipList) == 0 {
		return nil, ErrNotFound
	} else if len(vipList) > 1 {
		return nil, ErrMultipleResults
	}

	return &vipList[0], nil
}

func getLoadbalancerByName(client *gophercloud.ServiceClient, name string) (*loadbalancers.LoadBalancer, error) {
	opts := loadbalancers.ListOpts{
		Name: name,
	}
	pager := loadbalancers.List(client, opts)

	loadbalancerList := make([]loadbalancers.LoadBalancer, 0, 1)

	err := pager.EachPage(func(page pagination.Page) (bool, error) {
		v, err := loadbalancers.ExtractLoadBalancers(page)
		if err != nil {
			return false, err
		}
		loadbalancerList = append(loadbalancerList, v...)
		if len(loadbalancerList) > 1 {
			return false, ErrMultipleResults
		}
		return true, nil
	})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	if len(loadbalancerList) == 0 {
		return nil, ErrNotFound
	} else if len(loadbalancerList) > 1 {
		return nil, ErrMultipleResults
	}

	return &loadbalancerList[0], nil
}

func getListenersByLoadBalancerID(client *gophercloud.ServiceClient, id string) ([]listeners.Listener, error) {
	var existingListeners []listeners.Listener
	err := listeners.List(client, listeners.ListOpts{LoadbalancerID: id}).EachPage(func(page pagination.Page) (bool, error) {
		listenerList, err := listeners.ExtractListeners(page)
		if err != nil {
			return false, err
		}
		for _, l := range listenerList {
			for _, lb := range l.Loadbalancers {
				if lb.ID == id {
					existingListeners = append(existingListeners, l)
					break
				}
			}
		}

		return true, nil
	})
	if err != nil {
		return nil, err
	}

	return existingListeners, nil
}

// get listener for a port or nil if does not exist
func getListenerForPort(existingListeners []listeners.Listener, port v1.ServicePort) *listeners.Listener {
	for _, l := range existingListeners {
		if listeners.Protocol(l.Protocol) == toListenersProtocol(port.Protocol) && l.ProtocolPort == int(port.Port) {
			return &l
		}
	}

	return nil
}

// Get pool for a listener. A listener always has exactly one pool.
func getPoolByListenerID(client *gophercloud.ServiceClient, loadbalancerID string, listenerID string) (*v2pools.Pool, error) {
	listenerPools := make([]v2pools.Pool, 0, 1)
	err := v2pools.List(client, v2pools.ListOpts{LoadbalancerID: loadbalancerID}).EachPage(func(page pagination.Page) (bool, error) {
		poolsList, err := v2pools.ExtractPools(page)
		if err != nil {
			return false, err
		}
		for _, p := range poolsList {
			for _, l := range p.Listeners {
				if l.ID == listenerID {
					listenerPools = append(listenerPools, p)
				}
			}
		}
		if len(listenerPools) > 1 {
			return false, ErrMultipleResults
		}
		return true, nil
	})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	if len(listenerPools) == 0 {
		return nil, ErrNotFound
	} else if len(listenerPools) > 1 {
		return nil, ErrMultipleResults
	}

	return &listenerPools[0], nil
}

func getMembersByPoolID(client *gophercloud.ServiceClient, id string) ([]v2pools.Member, error) {
	var members []v2pools.Member
	err := v2pools.ListMembers(client, id, v2pools.ListMembersOpts{}).EachPage(func(page pagination.Page) (bool, error) {
		membersList, err := v2pools.ExtractMembers(page)
		if err != nil {
			return false, err
		}
		members = append(members, membersList...)

		return true, nil
	})
	if err != nil {
		return nil, err
	}

	return members, nil
}

// Each pool has exactly one or zero monitors. ListOpts does not seem to filter anything.
func getMonitorByPoolID(client *gophercloud.ServiceClient, id string) (*v2monitors.Monitor, error) {
	var monitorList []v2monitors.Monitor
	err := v2monitors.List(client, v2monitors.ListOpts{PoolID: id}).EachPage(func(page pagination.Page) (bool, error) {
		monitorsList, err := v2monitors.ExtractMonitors(page)
		if err != nil {
			return false, err
		}

		for _, monitor := range monitorsList {
			// bugfix, filter by poolid
			for _, pool := range monitor.Pools {
				if pool.ID == id {
					monitorList = append(monitorList, monitor)
				}
			}
		}
		if len(monitorList) > 1 {
			return false, ErrMultipleResults
		}
		return true, nil
	})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	if len(monitorList) == 0 {
		return nil, ErrNotFound
	} else if len(monitorList) > 1 {
		return nil, ErrMultipleResults
	}

	return &monitorList[0], nil
}

// Check if a member exists for node
func memberExists(members []v2pools.Member, addr string, port int) bool {
	for _, member := range members {
		if member.Address == addr && member.ProtocolPort == port {
			return true
		}
	}

	return false
}

func popListener(existingListeners []listeners.Listener, id string) []listeners.Listener {
	for i, existingListener := range existingListeners {
		if existingListener.ID == id {
			existingListeners[i] = existingListeners[len(existingListeners)-1]
			existingListeners = existingListeners[:len(existingListeners)-1]
			break
		}
	}

	return existingListeners
}

func popMember(members []v2pools.Member, addr string, port int) []v2pools.Member {
	for i, member := range members {
		if member.Address == addr && member.ProtocolPort == port {
			members[i] = members[len(members)-1]
			members = members[:len(members)-1]
		}
	}

	return members
}

func getSecurityGroupName(clusterName string, service *v1.Service) string {
	return fmt.Sprintf("lb-sg-%s-%v", clusterName, service.Name)
}

func getSecurityGroupRules(client *gophercloud.ServiceClient, opts rules.ListOpts) ([]rules.SecGroupRule, error) {

	pager := rules.List(client, opts)

	var securityRules []rules.SecGroupRule

	err := pager.EachPage(func(page pagination.Page) (bool, error) {
		ruleList, err := rules.ExtractRules(page)
		if err != nil {
			return false, err
		}
		securityRules = append(securityRules, ruleList...)
		return true, nil
	})

	if err != nil {
		return nil, err
	}

	return securityRules, nil
}

func waitLoadbalancerActiveProvisioningStatus(client *gophercloud.ServiceClient, loadbalancerID string) (string, error) {
	start := time.Now().Second()
	for {
		loadbalancer, err := loadbalancers.Get(client, loadbalancerID).Extract()
		if err != nil {
			return "", err
		}
		if loadbalancer.ProvisioningStatus == "ACTIVE" {
			return "ACTIVE", nil
		} else if loadbalancer.ProvisioningStatus == "ERROR" {
			return "ERROR", fmt.Errorf("Loadbalancer has gone into ERROR state")
		}

		time.Sleep(1 * time.Second)

		if time.Now().Second()-start >= loadbalancerActiveTimeoutSeconds {
			return loadbalancer.ProvisioningStatus, fmt.Errorf("Loadbalancer failed to go into ACTIVE provisioning status within alloted time")
		}
	}
}

func waitLoadbalancerDeleted(client *gophercloud.ServiceClient, loadbalancerID string) error {
	start := time.Now().Second()
	for {
		_, err := loadbalancers.Get(client, loadbalancerID).Extract()
		if err != nil {
			if err == ErrNotFound {
				return nil
			} else {
				return err
			}
		}

		time.Sleep(1 * time.Second)

		if time.Now().Second()-start >= loadbalancerDeleteTimeoutSeconds {
			return fmt.Errorf("Loadbalancer failed to delete within the alloted time")
		}

	}
}

func toRuleProtocol(protocol v1.Protocol) rules.RuleProtocol {
	switch protocol {
	case v1.ProtocolTCP:
		return rules.ProtocolTCP
	case v1.ProtocolUDP:
		return rules.ProtocolUDP
	default:
		return rules.RuleProtocol(strings.ToLower(string(protocol)))
	}
}

func toListenersProtocol(protocol v1.Protocol) listeners.Protocol {
	switch protocol {
	case v1.ProtocolTCP:
		return listeners.ProtocolTCP
	default:
		return listeners.Protocol(string(protocol))
	}
}

func createNodeSecurityGroup(client *gophercloud.ServiceClient, nodeSecurityGroupID string, port int, protocol v1.Protocol, lbSecGroup string) error {
	v4NodeSecGroupRuleCreateOpts := rules.CreateOpts{
		Direction:     rules.DirIngress,
		PortRangeMax:  port,
		PortRangeMin:  port,
		Protocol:      toRuleProtocol(protocol),
		RemoteGroupID: lbSecGroup,
		SecGroupID:    nodeSecurityGroupID,
		EtherType:     rules.EtherType4,
	}

	v6NodeSecGroupRuleCreateOpts := rules.CreateOpts{
		Direction:     rules.DirIngress,
		PortRangeMax:  port,
		PortRangeMin:  port,
		Protocol:      toRuleProtocol(protocol),
		RemoteGroupID: lbSecGroup,
		SecGroupID:    nodeSecurityGroupID,
		EtherType:     rules.EtherType6,
	}

	_, err := rules.Create(client, v4NodeSecGroupRuleCreateOpts).Extract()

	if err != nil {
		return err
	}

	_, err = rules.Create(client, v6NodeSecGroupRuleCreateOpts).Extract()

	if err != nil {
		return err
	}
	return nil
}

func (lbaas *LbaasV2) createLoadBalancer(service *v1.Service, name string) (*loadbalancers.LoadBalancer, error) {
	createOpts := loadbalancers.CreateOpts{
		Name:        name,
		Description: fmt.Sprintf("Kubernetes external service %s", name),
		VipSubnetID: lbaas.opts.SubnetId,
	}

	loadBalancerIP := service.Spec.LoadBalancerIP
	if loadBalancerIP != "" {
		createOpts.VipAddress = loadBalancerIP
	}

	loadbalancer, err := loadbalancers.Create(lbaas.network, createOpts).Extract()
	if err != nil {
		return nil, fmt.Errorf("Error creating loadbalancer %v: %v", createOpts, err)
	}
	return loadbalancer, nil
}

func stringInArray(x string, list []string) bool {
	for _, y := range list {
		if y == x {
			return true
		}
	}
	return false
}

func (lbaas *LbaasV2) GetLoadBalancer(clusterName string, service *v1.Service) (*v1.LoadBalancerStatus, bool, error) {
	loadBalancerName := cloudprovider.GetLoadBalancerName(service)
	loadbalancer, err := getLoadbalancerByName(lbaas.network, loadBalancerName)
	if err == ErrNotFound {
		return nil, false, nil
	}
	if loadbalancer == nil {
		return nil, false, err
	}

	status := &v1.LoadBalancerStatus{}
	status.Ingress = []v1.LoadBalancerIngress{{IP: loadbalancer.VipAddress}}

	return status, true, err
}

// The LB needs to be configured with instance addresses on the same
// subnet as the LB (aka opts.SubnetId).  Currently we're just
// guessing that the node's InternalIP is the right address - and that
// should be sufficient for all "normal" cases.
func nodeAddressForLB(node *v1.Node) (string, error) {
	addrs := node.Status.Addresses
	if len(addrs) == 0 {
		return "", ErrNoAddressFound
	}

	for _, addr := range addrs {
		if addr.Type == v1.NodeInternalIP {
			return addr.Address, nil
		}
	}

	return addrs[0].Address, nil
}

// TODO: This code currently ignores 'region' and always creates a
// loadbalancer in only the current OpenStack region.  We should take
// a list of regions (from config) and query/create loadbalancers in
// each region.

func (lbaas *LbaasV2) EnsureLoadBalancer(clusterName string, apiService *v1.Service, nodes []*v1.Node) (*v1.LoadBalancerStatus, error) {
	glog.V(4).Infof("EnsureLoadBalancer(%v, %v, %v, %v, %v, %v, %v)", clusterName, apiService.Namespace, apiService.Name, apiService.Spec.LoadBalancerIP, apiService.Spec.Ports, nodes, apiService.Annotations)

	ports := apiService.Spec.Ports
	if len(ports) == 0 {
		return nil, fmt.Errorf("no ports provided to openstack load balancer")
	}

	// Check for TCP protocol on each port
	// TODO: Convert all error messages to use an event recorder
	for _, port := range ports {
		if port.Protocol != v1.ProtocolTCP {
			return nil, fmt.Errorf("Only TCP LoadBalancer is supported for openstack load balancers")
		}
	}

	sourceRanges, err := service.GetLoadBalancerSourceRanges(apiService)
	if err != nil {
		return nil, err
	}

	if !service.IsAllowAll(sourceRanges) && !lbaas.opts.ManageSecurityGroups {
		return nil, fmt.Errorf("Source range restrictions are not supported for openstack load balancers without managing security groups")
	}

	affinity := v1.ServiceAffinityNone
	var persistence *v2pools.SessionPersistence
	switch affinity {
	case v1.ServiceAffinityNone:
		persistence = nil
	case v1.ServiceAffinityClientIP:
		persistence = &v2pools.SessionPersistence{Type: "SOURCE_IP"}
	default:
		return nil, fmt.Errorf("unsupported load balancer affinity: %v", affinity)
	}

	name := cloudprovider.GetLoadBalancerName(apiService)
	loadbalancer, err := getLoadbalancerByName(lbaas.network, name)
	if err != nil {
		if err != ErrNotFound {
			return nil, fmt.Errorf("Error getting loadbalancer %s: %v", name, err)
		}
		glog.V(2).Infof("Creating loadbalancer %s", name)
		loadbalancer, err = lbaas.createLoadBalancer(apiService, name)
		if err != nil {
			// Unknown error, retry later
			return nil, fmt.Errorf("Error creating loadbalancer %s: %v", name, err)
		}
	} else {
		glog.V(2).Infof("LoadBalancer %s already exists", name)
	}

	waitLoadbalancerActiveProvisioningStatus(lbaas.network, loadbalancer.ID)

	lbmethod := v2pools.LBMethod(lbaas.opts.LBMethod)
	if lbmethod == "" {
		lbmethod = v2pools.LBMethodRoundRobin
	}

	oldListeners, err := getListenersByLoadBalancerID(lbaas.network, loadbalancer.ID)
	if err != nil {
		return nil, fmt.Errorf("Error getting LB %s listeners: %v", name, err)
	}
	for portIndex, port := range ports {
		listener := getListenerForPort(oldListeners, port)
		if listener == nil {
			glog.V(4).Infof("Creating listener for port %d", int(port.Port))
			listener, err = listeners.Create(lbaas.network, listeners.CreateOpts{
				Name:           fmt.Sprintf("listener_%s_%d", name, portIndex),
				Protocol:       listeners.Protocol(port.Protocol),
				ProtocolPort:   int(port.Port),
				LoadbalancerID: loadbalancer.ID,
			}).Extract()
			if err != nil {
				// Unknown error, retry later
				return nil, fmt.Errorf("Error creating LB listener: %v", err)
			}
			waitLoadbalancerActiveProvisioningStatus(lbaas.network, loadbalancer.ID)
		}

		glog.V(4).Infof("Listener for %s port %d: %s", string(port.Protocol), int(port.Port), listener.ID)

		// After all ports have been processed, remaining listeners are removed as obsolete.
		// Pop valid listeners.
		oldListeners = popListener(oldListeners, listener.ID)
		pool, err := getPoolByListenerID(lbaas.network, loadbalancer.ID, listener.ID)
		if err != nil && err != ErrNotFound {
			// Unknown error, retry later
			return nil, fmt.Errorf("Error getting pool for listener %s: %v", listener.ID, err)
		}
		if pool == nil {
			glog.V(4).Infof("Creating pool for listener %s", listener.ID)
			pool, err = v2pools.Create(lbaas.network, v2pools.CreateOpts{
				Name:        fmt.Sprintf("pool_%s_%d", name, portIndex),
				Protocol:    v2pools.Protocol(port.Protocol),
				LBMethod:    lbmethod,
				ListenerID:  listener.ID,
				Persistence: persistence,
			}).Extract()
			if err != nil {
				// Unknown error, retry later
				return nil, fmt.Errorf("Error creating pool for listener %s: %v", listener.ID, err)
			}
			waitLoadbalancerActiveProvisioningStatus(lbaas.network, loadbalancer.ID)
		}

		glog.V(4).Infof("Pool for listener %s: %s", listener.ID, pool.ID)
		members, err := getMembersByPoolID(lbaas.network, pool.ID)
		if err != nil && !isNotFound(err) {
			return nil, fmt.Errorf("Error getting pool members %s: %v", pool.ID, err)
		}
		for _, node := range nodes {
			addr, err := nodeAddressForLB(node)
			if err != nil {
				if err == ErrNotFound {
					// Node failure, do not create member
					glog.Warningf("Failed to create LB pool member for node %s: %v", node.Name, err)
					continue
				} else {
					return nil, fmt.Errorf("Error getting address for node %s: %v", node.Name, err)
				}
			}

			if !memberExists(members, addr, int(port.NodePort)) {
				glog.V(4).Infof("Creating member for pool %s", pool.ID)
				_, err := v2pools.CreateMember(lbaas.network, pool.ID, v2pools.CreateMemberOpts{
					ProtocolPort: int(port.NodePort),
					Address:      addr,
					SubnetID:     lbaas.opts.SubnetId,
				}).Extract()
				if err != nil {
					return nil, fmt.Errorf("Error creating LB pool member for node: %s, %v", node.Name, err)
				}

				waitLoadbalancerActiveProvisioningStatus(lbaas.network, loadbalancer.ID)
			} else {
				// After all members have been processed, remaining members are deleted as obsolete.
				members = popMember(members, addr, int(port.NodePort))
			}

			glog.V(4).Infof("Ensured pool %s has member for %s at %s", pool.ID, node.Name, addr)
		}

		// Delete obsolete members for this pool
		for _, member := range members {
			glog.V(4).Infof("Deleting obsolete member %s for pool %s address %s", member.ID, pool.ID, member.Address)
			err := v2pools.DeleteMember(lbaas.network, pool.ID, member.ID).ExtractErr()
			if err != nil && !isNotFound(err) {
				return nil, fmt.Errorf("Error deleting obsolete member %s for pool %s address %s: %v", member.ID, pool.ID, member.Address, err)
			}
			waitLoadbalancerActiveProvisioningStatus(lbaas.network, loadbalancer.ID)
		}

		monitorID := pool.MonitorID
		if monitorID == "" && lbaas.opts.CreateMonitor {
			glog.V(4).Infof("Creating monitor for pool %s", pool.ID)
			monitor, err := v2monitors.Create(lbaas.network, v2monitors.CreateOpts{
				PoolID:     pool.ID,
				Type:       string(port.Protocol),
				Delay:      int(lbaas.opts.MonitorDelay.Duration.Seconds()),
				Timeout:    int(lbaas.opts.MonitorTimeout.Duration.Seconds()),
				MaxRetries: int(lbaas.opts.MonitorMaxRetries),
			}).Extract()
			if err != nil {
				return nil, fmt.Errorf("Error creating LB pool healthmonitor: %v", err)
			}
			waitLoadbalancerActiveProvisioningStatus(lbaas.network, loadbalancer.ID)
			monitorID = monitor.ID
		}

		glog.V(4).Infof("Monitor for pool %s: %s", pool.ID, monitorID)
	}

	// All remaining listeners are obsolete, delete
	for _, listener := range oldListeners {
		glog.V(4).Infof("Deleting obsolete listener %s:", listener.ID)
		// get pool for listener
		pool, err := getPoolByListenerID(lbaas.network, loadbalancer.ID, listener.ID)
		if err != nil && err != ErrNotFound {
			return nil, fmt.Errorf("Error getting pool for obsolete listener %s: %v", listener.ID, err)
		}
		if pool != nil {
			// get and delete monitor
			monitorID := pool.MonitorID
			if monitorID != "" {
				glog.V(4).Infof("Deleting obsolete monitor %s for pool %s", monitorID, pool.ID)
				err = v2monitors.Delete(lbaas.network, monitorID).ExtractErr()
				if err != nil && !isNotFound(err) {
					return nil, fmt.Errorf("Error deleting obsolete monitor %s for pool %s: %v", monitorID, pool.ID, err)
				}
				waitLoadbalancerActiveProvisioningStatus(lbaas.network, loadbalancer.ID)
			}
			// get and delete pool members
			members, err := getMembersByPoolID(lbaas.network, pool.ID)
			if err != nil && !isNotFound(err) {
				return nil, fmt.Errorf("Error getting members for pool %s: %v", pool.ID, err)
			}
			if members != nil {
				for _, member := range members {
					glog.V(4).Infof("Deleting obsolete member %s for pool %s address %s", member.ID, pool.ID, member.Address)
					err := v2pools.DeleteMember(lbaas.network, pool.ID, member.ID).ExtractErr()
					if err != nil && !isNotFound(err) {
						return nil, fmt.Errorf("Error deleting obsolete member %s for pool %s address %s: %v", member.ID, pool.ID, member.Address, err)
					}
					waitLoadbalancerActiveProvisioningStatus(lbaas.network, loadbalancer.ID)
				}
			}
			glog.V(4).Infof("Deleting obsolete pool %s for listener %s", pool.ID, listener.ID)
			// delete pool
			err = v2pools.Delete(lbaas.network, pool.ID).ExtractErr()
			if err != nil && !isNotFound(err) {
				return nil, fmt.Errorf("Error deleting obsolete pool %s for listener %s: %v", pool.ID, listener.ID, err)
			}
			waitLoadbalancerActiveProvisioningStatus(lbaas.network, loadbalancer.ID)
		}
		// delete listener
		err = listeners.Delete(lbaas.network, listener.ID).ExtractErr()
		if err != nil && !isNotFound(err) {
			return nil, fmt.Errorf("Error deleteting obsolete listener: %v", err)
		}
		waitLoadbalancerActiveProvisioningStatus(lbaas.network, loadbalancer.ID)
		glog.V(2).Infof("Deleted obsolete listener: %s", listener.ID)
	}

	status := &v1.LoadBalancerStatus{}

	status.Ingress = []v1.LoadBalancerIngress{{IP: loadbalancer.VipAddress}}

	port, err := getPortByIP(lbaas.network, loadbalancer.VipAddress)
	if err != nil {
		return nil, fmt.Errorf("Error getting port for LB vip %s: %v", loadbalancer.VipAddress, err)
	}
	floatIP, err := getFloatingIPByPortID(lbaas.network, port.ID)
	if err != nil && err != ErrNotFound {
		return nil, fmt.Errorf("Error getting floating ip for port %s: %v", port.ID, err)
	}
	if floatIP == nil && lbaas.opts.FloatingNetworkId != "" {
		glog.V(4).Infof("Creating floating ip for loadbalancer %s port %s", loadbalancer.ID, port.ID)
		floatIPOpts := floatingips.CreateOpts{
			FloatingNetworkID: lbaas.opts.FloatingNetworkId,
			PortID:            port.ID,
		}
		floatIP, err = floatingips.Create(lbaas.network, floatIPOpts).Extract()
		if err != nil {
			return nil, fmt.Errorf("Error creating LB floatingip %+v: %v", floatIPOpts, err)
		}
	}
	if floatIP != nil {
		status.Ingress = append(status.Ingress, v1.LoadBalancerIngress{IP: floatIP.FloatingIP})
	}

	if lbaas.opts.ManageSecurityGroups {
		lbSecGroupCreateOpts := groups.CreateOpts{
			Name:        getSecurityGroupName(clusterName, apiService),
			Description: fmt.Sprintf("Securty Group for %v Service LoadBalancer", apiService.Name),
		}

		lbSecGroup, err := groups.Create(lbaas.network, lbSecGroupCreateOpts).Extract()

		if err != nil {
			// cleanup what was created so far
			_ = lbaas.EnsureLoadBalancerDeleted(clusterName, apiService)
			return nil, err
		}

		for _, port := range ports {

			for _, sourceRange := range sourceRanges.StringSlice() {
				ethertype := rules.EtherType4
				network, _, err := net.ParseCIDR(sourceRange)

				if err != nil {
					// cleanup what was created so far
					glog.Errorf("Error parsing source range %s as a CIDR", sourceRange)
					_ = lbaas.EnsureLoadBalancerDeleted(clusterName, apiService)
					return nil, err
				}

				if network.To4() == nil {
					ethertype = rules.EtherType6
				}

				lbSecGroupRuleCreateOpts := rules.CreateOpts{
					Direction:      rules.DirIngress,
					PortRangeMax:   int(port.Port),
					PortRangeMin:   int(port.Port),
					Protocol:       toRuleProtocol(port.Protocol),
					RemoteIPPrefix: sourceRange,
					SecGroupID:     lbSecGroup.ID,
					EtherType:      ethertype,
				}

				_, err = rules.Create(lbaas.network, lbSecGroupRuleCreateOpts).Extract()

				if err != nil {
					// cleanup what was created so far
					_ = lbaas.EnsureLoadBalancerDeleted(clusterName, apiService)
					return nil, err
				}
			}

			err := createNodeSecurityGroup(lbaas.network, lbaas.opts.NodeSecurityGroupID, int(port.NodePort), port.Protocol, lbSecGroup.ID)
			if err != nil {
				glog.Errorf("Error occured creating security group for loadbalancer %s:", loadbalancer.ID)
				_ = lbaas.EnsureLoadBalancerDeleted(clusterName, apiService)
				return nil, err
			}
		}

		lbSecGroupRuleCreateOpts := rules.CreateOpts{
			Direction:      rules.DirIngress,
			PortRangeMax:   4, // ICMP: Code -  Values for ICMP  "Destination Unreachable: Fragmentation Needed and Don't Fragment was Set"
			PortRangeMin:   3, // ICMP: Type
			Protocol:       rules.ProtocolICMP,
			RemoteIPPrefix: "0.0.0.0/0", // The Fragmentation packet can come from anywhere along the path back to the sourceRange - we need to all this from all
			SecGroupID:     lbSecGroup.ID,
			EtherType:      rules.EtherType4,
		}

		_, err = rules.Create(lbaas.network, lbSecGroupRuleCreateOpts).Extract()

		if err != nil {
			// cleanup what was created so far
			_ = lbaas.EnsureLoadBalancerDeleted(clusterName, apiService)
			return nil, err
		}

		lbSecGroupRuleCreateOpts = rules.CreateOpts{
			Direction:      rules.DirIngress,
			PortRangeMax:   0, // ICMP: Code - Values for ICMP "Packet Too Big"
			PortRangeMin:   2, // ICMP: Type
			Protocol:       rules.ProtocolICMP,
			RemoteIPPrefix: "::/0", // The Fragmentation packet can come from anywhere along the path back to the sourceRange - we need to all this from all
			SecGroupID:     lbSecGroup.ID,
			EtherType:      rules.EtherType6,
		}

		_, err = rules.Create(lbaas.network, lbSecGroupRuleCreateOpts).Extract()

		if err != nil {
			// cleanup what was created so far
			_ = lbaas.EnsureLoadBalancerDeleted(clusterName, apiService)
			return nil, err
		}

		// Get the port ID
		port, err := getPortByIP(lbaas.network, loadbalancer.VipAddress)
		if err != nil {
			// cleanup what was created so far
			_ = lbaas.EnsureLoadBalancerDeleted(clusterName, apiService)
			return nil, err
		}

		update_opts := neutronports.UpdateOpts{SecurityGroups: []string{lbSecGroup.ID}}

		res := neutronports.Update(lbaas.network, port.ID, update_opts)

		if res.Err != nil {
			glog.Errorf("Error occured updating port: %s", port.ID)
			// cleanup what was created so far
			_ = lbaas.EnsureLoadBalancerDeleted(clusterName, apiService)
			return nil, res.Err
		}

	}

	return status, nil
}

func (lbaas *LbaasV2) UpdateLoadBalancer(clusterName string, service *v1.Service, nodes []*v1.Node) error {
	loadBalancerName := cloudprovider.GetLoadBalancerName(service)
	glog.V(4).Infof("UpdateLoadBalancer(%v, %v, %v)", clusterName, loadBalancerName, nodes)

	ports := service.Spec.Ports
	if len(ports) == 0 {
		return fmt.Errorf("no ports provided to openstack load balancer")
	}

	loadbalancer, err := getLoadbalancerByName(lbaas.network, loadBalancerName)
	if err != nil {
		return err
	}
	if loadbalancer == nil {
		return fmt.Errorf("Loadbalancer %s does not exist", loadBalancerName)
	}

	// Get all listeners for this loadbalancer, by "port key".
	type portKey struct {
		Protocol listeners.Protocol
		Port     int
	}

	lbListeners := make(map[portKey]listeners.Listener)
	err = listeners.List(lbaas.network, listeners.ListOpts{LoadbalancerID: loadbalancer.ID}).EachPage(func(page pagination.Page) (bool, error) {
		listenersList, err := listeners.ExtractListeners(page)
		if err != nil {
			return false, err
		}
		for _, l := range listenersList {
			for _, lb := range l.Loadbalancers {
				// Double check this Listener belongs to the LB we're updating. Neutron's API filtering
				// can't be counted on in older releases (i.e Liberty).
				if loadbalancer.ID == lb.ID {
					key := portKey{Protocol: listeners.Protocol(l.Protocol), Port: l.ProtocolPort}
					lbListeners[key] = l
					break
				}
			}
		}
		return true, nil
	})
	if err != nil {
		return err
	}

	// Get all pools for this loadbalancer, by listener ID.
	lbPools := make(map[string]v2pools.Pool)
	err = v2pools.List(lbaas.network, v2pools.ListOpts{LoadbalancerID: loadbalancer.ID}).EachPage(func(page pagination.Page) (bool, error) {
		poolsList, err := v2pools.ExtractPools(page)
		if err != nil {
			return false, err
		}
		for _, p := range poolsList {
			for _, l := range p.Listeners {
				// Double check this Pool belongs to the LB we're deleting. Neutron's API filtering
				// can't be counted on in older releases (i.e Liberty).
				for _, val := range lbListeners {
					if val.ID == l.ID {
						lbPools[l.ID] = p
						break
					}
				}
			}
		}
		return true, nil
	})
	if err != nil {
		return err
	}

	// Compose Set of member (addresses) that _should_ exist
	addrs := map[string]empty{}
	for _, node := range nodes {
		addr, err := nodeAddressForLB(node)
		if err != nil {
			return err
		}
		addrs[addr] = empty{}
	}

	// Check for adding/removing members associated with each port
	for _, port := range ports {
		// Get listener associated with this port
		listener, ok := lbListeners[portKey{
			Protocol: toListenersProtocol(port.Protocol),
			Port:     int(port.Port),
		}]
		if !ok {
			return fmt.Errorf("Loadbalancer %s does not contain required listener for port %d and protocol %s", loadBalancerName, port.Port, port.Protocol)
		}

		// Get pool associated with this listener
		pool, ok := lbPools[listener.ID]
		if !ok {
			return fmt.Errorf("Loadbalancer %s does not contain required pool for listener %s", loadBalancerName, listener.ID)
		}

		// Find existing pool members (by address) for this port
		members := make(map[string]v2pools.Member)
		err := v2pools.ListMembers(lbaas.network, pool.ID, v2pools.ListMembersOpts{}).EachPage(func(page pagination.Page) (bool, error) {
			membersList, err := v2pools.ExtractMembers(page)
			if err != nil {
				return false, err
			}
			for _, member := range membersList {
				members[member.Address] = member
			}
			return true, nil
		})
		if err != nil {
			return err
		}

		// Add any new members for this port
		for addr := range addrs {
			if _, ok := members[addr]; ok && members[addr].ProtocolPort == int(port.NodePort) {
				// Already exists, do not create member
				continue
			}
			_, err := v2pools.CreateMember(lbaas.network, pool.ID, v2pools.CreateMemberOpts{
				Address:      addr,
				ProtocolPort: int(port.NodePort),
				SubnetID:     lbaas.opts.SubnetId,
			}).Extract()
			if err != nil {
				return err
			}
			waitLoadbalancerActiveProvisioningStatus(lbaas.network, loadbalancer.ID)
		}

		// Remove any old members for this port
		for _, member := range members {
			if _, ok := addrs[member.Address]; ok && member.ProtocolPort == int(port.NodePort) {
				// Still present, do not delete member
				continue
			}
			err = v2pools.DeleteMember(lbaas.network, pool.ID, member.ID).ExtractErr()
			if err != nil && !isNotFound(err) {
				return err
			}
			waitLoadbalancerActiveProvisioningStatus(lbaas.network, loadbalancer.ID)
		}
	}
	return nil
}

func (lbaas *LbaasV2) EnsureLoadBalancerDeleted(clusterName string, service *v1.Service) error {
	loadBalancerName := cloudprovider.GetLoadBalancerName(service)
	glog.V(4).Infof("EnsureLoadBalancerDeleted(%v, %v)", clusterName, loadBalancerName)

	loadbalancer, err := getLoadbalancerByName(lbaas.network, loadBalancerName)
	if err != nil && err != ErrNotFound {
		return err
	}
	if loadbalancer == nil {
		return nil
	}

	if lbaas.opts.FloatingNetworkId != "" && loadbalancer != nil {
		port, err := getPortByIP(lbaas.network, loadbalancer.VipAddress)
		if err != nil {
			return err
		}

		floatingIP, err := getFloatingIPByPortID(lbaas.network, port.ID)
		if err != nil && err != ErrNotFound {
			return err
		}
		if floatingIP != nil {
			err = floatingips.Delete(lbaas.network, floatingIP.ID).ExtractErr()
			if err != nil && !isNotFound(err) {
				return err
			}
		}
	}

	// get all listeners associated with this loadbalancer
	var listenerIDs []string
	err = listeners.List(lbaas.network, listeners.ListOpts{LoadbalancerID: loadbalancer.ID}).EachPage(func(page pagination.Page) (bool, error) {
		listenerList, err := listeners.ExtractListeners(page)
		if err != nil {
			return false, err
		}

		for _, listener := range listenerList {
			listenerIDs = append(listenerIDs, listener.ID)
		}

		return true, nil
	})
	if err != nil {
		return err
	}

	// get all pools (and health monitors) associated with this loadbalancer
	var poolIDs []string
	var monitorIDs []string
	err = v2pools.List(lbaas.network, v2pools.ListOpts{LoadbalancerID: loadbalancer.ID}).EachPage(func(page pagination.Page) (bool, error) {
		poolsList, err := v2pools.ExtractPools(page)
		if err != nil {
			return false, err
		}

		for _, pool := range poolsList {
			poolIDs = append(poolIDs, pool.ID)
			monitorIDs = append(monitorIDs, pool.MonitorID)
		}

		return true, nil
	})
	if err != nil {
		return err
	}

	// get all members associated with each poolIDs
	var memberIDs []string
	for _, poolID := range poolIDs {
		err := v2pools.ListMembers(lbaas.network, poolID, v2pools.ListMembersOpts{}).EachPage(func(page pagination.Page) (bool, error) {
			membersList, err := v2pools.ExtractMembers(page)
			if err != nil {
				return false, err
			}

			for _, member := range membersList {
				memberIDs = append(memberIDs, member.ID)
			}

			return true, nil
		})
		if err != nil {
			return err
		}
	}

	// delete all monitors
	for _, monitorID := range monitorIDs {
		err := v2monitors.Delete(lbaas.network, monitorID).ExtractErr()
		if err != nil && !isNotFound(err) {
			return err
		}
		waitLoadbalancerActiveProvisioningStatus(lbaas.network, loadbalancer.ID)
	}

	// delete all members and pools
	for _, poolID := range poolIDs {
		// delete all members for this pool
		for _, memberID := range memberIDs {
			err := v2pools.DeleteMember(lbaas.network, poolID, memberID).ExtractErr()
			if err != nil && !isNotFound(err) {
				return err
			}
			waitLoadbalancerActiveProvisioningStatus(lbaas.network, loadbalancer.ID)
		}

		// delete pool
		err := v2pools.Delete(lbaas.network, poolID).ExtractErr()
		if err != nil && !isNotFound(err) {
			return err
		}
		waitLoadbalancerActiveProvisioningStatus(lbaas.network, loadbalancer.ID)
	}

	// delete all listeners
	for _, listenerID := range listenerIDs {
		err := listeners.Delete(lbaas.network, listenerID).ExtractErr()
		if err != nil && !isNotFound(err) {
			return err
		}
		waitLoadbalancerActiveProvisioningStatus(lbaas.network, loadbalancer.ID)
	}

	// delete loadbalancer
	err = loadbalancers.Delete(lbaas.network, loadbalancer.ID).ExtractErr()
	if err != nil && !isNotFound(err) {
		return err
	}
	waitLoadbalancerDeleted(lbaas.network, loadbalancer.ID)

	// Delete the Security Group
	if lbaas.opts.ManageSecurityGroups {
		// Generate Name
		lbSecGroupName := getSecurityGroupName(clusterName, service)
		lbSecGroupID, err := groups.IDFromName(lbaas.network, lbSecGroupName)
		if err != nil {
			glog.V(1).Infof("Error occurred finding security group: %s: %v", lbSecGroupName, err)
			return nil
		}

		lbSecGroup := groups.Delete(lbaas.network, lbSecGroupID)
		if lbSecGroup.Err != nil && !isNotFound(lbSecGroup.Err) {
			return lbSecGroup.Err
		}

		// Delete the rules in the Node Security Group
		opts := rules.ListOpts{
			SecGroupID:    lbaas.opts.NodeSecurityGroupID,
			RemoteGroupID: lbSecGroupID,
		}
		secGroupRules, err := getSecurityGroupRules(lbaas.network, opts)

		if err != nil && !isNotFound(err) {
			glog.Errorf("Error finding rules for remote group id %s in security group id %s", lbSecGroupID, lbaas.opts.NodeSecurityGroupID)
			return err
		}

		for _, rule := range secGroupRules {
			res := rules.Delete(lbaas.network, rule.ID)
			if res.Err != nil && !isNotFound(res.Err) {
				glog.V(1).Infof("Error occurred deleting security group rule: %s: %v", rule.ID, res.Err)
			}
		}
	}

	return nil
}

func (lb *LbaasV1) GetLoadBalancer(clusterName string, service *v1.Service) (*v1.LoadBalancerStatus, bool, error) {
	loadBalancerName := cloudprovider.GetLoadBalancerName(service)
	vip, err := getVipByName(lb.network, loadBalancerName)
	if err == ErrNotFound {
		return nil, false, nil
	}
	if vip == nil {
		return nil, false, err
	}

	status := &v1.LoadBalancerStatus{}
	status.Ingress = []v1.LoadBalancerIngress{{IP: vip.Address}}

	return status, true, err
}

// TODO: This code currently ignores 'region' and always creates a
// loadbalancer in only the current OpenStack region.  We should take
// a list of regions (from config) and query/create loadbalancers in
// each region.

func (lb *LbaasV1) EnsureLoadBalancer(clusterName string, apiService *v1.Service, nodes []*v1.Node) (*v1.LoadBalancerStatus, error) {
	glog.V(4).Infof("EnsureLoadBalancer(%v, %v, %v, %v, %v, %v, %v)", clusterName, apiService.Namespace, apiService.Name, apiService.Spec.LoadBalancerIP, apiService.Spec.Ports, nodes, apiService.Annotations)

	ports := apiService.Spec.Ports
	if len(ports) > 1 {
		return nil, fmt.Errorf("multiple ports are not supported in openstack v1 load balancers")
	} else if len(ports) == 0 {
		return nil, fmt.Errorf("no ports provided to openstack load balancer")
	}

	// The service controller verified all the protocols match on the ports, just check and use the first one
	// TODO: Convert all error messages to use an event recorder
	if ports[0].Protocol != v1.ProtocolTCP {
		return nil, fmt.Errorf("Only TCP LoadBalancer is supported for openstack load balancers")
	}

	affinity := apiService.Spec.SessionAffinity
	var persistence *vips.SessionPersistence
	switch affinity {
	case v1.ServiceAffinityNone:
		persistence = nil
	case v1.ServiceAffinityClientIP:
		persistence = &vips.SessionPersistence{Type: "SOURCE_IP"}
	default:
		return nil, fmt.Errorf("unsupported load balancer affinity: %v", affinity)
	}

	sourceRanges, err := service.GetLoadBalancerSourceRanges(apiService)
	if err != nil {
		return nil, err
	}

	if !service.IsAllowAll(sourceRanges) {
		return nil, fmt.Errorf("Source range restrictions are not supported for openstack load balancers")
	}

	glog.V(2).Infof("Checking if openstack load balancer already exists: %s", cloudprovider.GetLoadBalancerName(apiService))
	_, exists, err := lb.GetLoadBalancer(clusterName, apiService)
	if err != nil {
		return nil, fmt.Errorf("error checking if openstack load balancer already exists: %v", err)
	}

	// TODO: Implement a more efficient update strategy for common changes than delete & create
	// In particular, if we implement hosts update, we can get rid of UpdateHosts
	if exists {
		err := lb.EnsureLoadBalancerDeleted(clusterName, apiService)
		if err != nil {
			return nil, fmt.Errorf("error deleting existing openstack load balancer: %v", err)
		}
	}

	lbmethod := pools.LBMethod(lb.opts.LBMethod)
	if lbmethod == "" {
		lbmethod = pools.LBMethodRoundRobin
	}
	name := cloudprovider.GetLoadBalancerName(apiService)
	pool, err := pools.Create(lb.network, pools.CreateOpts{
		Name:     name,
		Protocol: pools.ProtocolTCP,
		SubnetID: lb.opts.SubnetId,
		LBMethod: lbmethod,
	}).Extract()
	if err != nil {
		return nil, err
	}

	for _, node := range nodes {
		addr, err := nodeAddressForLB(node)
		if err != nil {
			return nil, err
		}

		_, err = members.Create(lb.network, members.CreateOpts{
			PoolID:       pool.ID,
			ProtocolPort: int(ports[0].NodePort), //Note: only handles single port
			Address:      addr,
		}).Extract()
		if err != nil {
			pools.Delete(lb.network, pool.ID)
			return nil, err
		}
	}

	var mon *monitors.Monitor
	if lb.opts.CreateMonitor {
		mon, err = monitors.Create(lb.network, monitors.CreateOpts{
			Type:       monitors.TypeTCP,
			Delay:      int(lb.opts.MonitorDelay.Duration.Seconds()),
			Timeout:    int(lb.opts.MonitorTimeout.Duration.Seconds()),
			MaxRetries: int(lb.opts.MonitorMaxRetries),
		}).Extract()
		if err != nil {
			pools.Delete(lb.network, pool.ID)
			return nil, err
		}

		_, err = pools.AssociateMonitor(lb.network, pool.ID, mon.ID).Extract()
		if err != nil {
			monitors.Delete(lb.network, mon.ID)
			pools.Delete(lb.network, pool.ID)
			return nil, err
		}
	}

	createOpts := vips.CreateOpts{
		Name:         name,
		Description:  fmt.Sprintf("Kubernetes external service %s", name),
		Protocol:     "TCP",
		ProtocolPort: int(ports[0].Port), //TODO: need to handle multi-port
		PoolID:       pool.ID,
		SubnetID:     lb.opts.SubnetId,
		Persistence:  persistence,
	}

	loadBalancerIP := apiService.Spec.LoadBalancerIP
	if loadBalancerIP != "" {
		createOpts.Address = loadBalancerIP
	}

	vip, err := vips.Create(lb.network, createOpts).Extract()
	if err != nil {
		if mon != nil {
			monitors.Delete(lb.network, mon.ID)
		}
		pools.Delete(lb.network, pool.ID)
		return nil, err
	}

	status := &v1.LoadBalancerStatus{}

	status.Ingress = []v1.LoadBalancerIngress{{IP: vip.Address}}

	if lb.opts.FloatingNetworkId != "" {
		floatIPOpts := floatingips.CreateOpts{
			FloatingNetworkID: lb.opts.FloatingNetworkId,
			PortID:            vip.PortID,
		}
		floatIP, err := floatingips.Create(lb.network, floatIPOpts).Extract()
		if err != nil {
			return nil, err
		}

		status.Ingress = append(status.Ingress, v1.LoadBalancerIngress{IP: floatIP.FloatingIP})
	}

	return status, nil

}

func (lb *LbaasV1) UpdateLoadBalancer(clusterName string, service *v1.Service, nodes []*v1.Node) error {
	loadBalancerName := cloudprovider.GetLoadBalancerName(service)
	glog.V(4).Infof("UpdateLoadBalancer(%v, %v, %v)", clusterName, loadBalancerName, nodes)

	vip, err := getVipByName(lb.network, loadBalancerName)
	if err != nil {
		return err
	}

	// Set of member (addresses) that _should_ exist
	addrs := map[string]bool{}
	for _, node := range nodes {
		addr, err := nodeAddressForLB(node)
		if err != nil {
			return err
		}

		addrs[addr] = true
	}

	// Iterate over members that _do_ exist
	pager := members.List(lb.network, members.ListOpts{PoolID: vip.PoolID})
	err = pager.EachPage(func(page pagination.Page) (bool, error) {
		memList, err := members.ExtractMembers(page)
		if err != nil {
			return false, err
		}

		for _, member := range memList {
			if _, found := addrs[member.Address]; found {
				// Member already exists
				delete(addrs, member.Address)
			} else {
				// Member needs to be deleted
				err = members.Delete(lb.network, member.ID).ExtractErr()
				if err != nil {
					return false, err
				}
			}
		}

		return true, nil
	})
	if err != nil {
		return err
	}

	// Anything left in addrs is a new member that needs to be added
	for addr := range addrs {
		_, err := members.Create(lb.network, members.CreateOpts{
			PoolID:       vip.PoolID,
			Address:      addr,
			ProtocolPort: vip.ProtocolPort,
		}).Extract()
		if err != nil {
			return err
		}
	}

	return nil
}

func (lb *LbaasV1) EnsureLoadBalancerDeleted(clusterName string, service *v1.Service) error {
	loadBalancerName := cloudprovider.GetLoadBalancerName(service)
	glog.V(4).Infof("EnsureLoadBalancerDeleted(%v, %v)", clusterName, loadBalancerName)

	vip, err := getVipByName(lb.network, loadBalancerName)
	if err != nil && err != ErrNotFound {
		return err
	}

	if lb.opts.FloatingNetworkId != "" && vip != nil {
		floatingIP, err := getFloatingIPByPortID(lb.network, vip.PortID)
		if err != nil && !isNotFound(err) {
			return err
		}
		if floatingIP != nil {
			err = floatingips.Delete(lb.network, floatingIP.ID).ExtractErr()
			if err != nil && !isNotFound(err) {
				return err
			}
		}
	}

	// We have to delete the VIP before the pool can be deleted,
	// so no point continuing if this fails.
	if vip != nil {
		err := vips.Delete(lb.network, vip.ID).ExtractErr()
		if err != nil && !isNotFound(err) {
			return err
		}
	}

	var pool *pools.Pool
	if vip != nil {
		pool, err = pools.Get(lb.network, vip.PoolID).Extract()
		if err != nil && !isNotFound(err) {
			return err
		}
	} else {
		// The VIP is gone, but it is conceivable that a Pool
		// still exists that we failed to delete on some
		// previous occasion.  Make a best effort attempt to
		// cleanup any pools with the same name as the VIP.
		pool, err = getPoolByName(lb.network, service.Name)
		if err != nil && err != ErrNotFound {
			return err
		}
	}

	if pool != nil {
		for _, monId := range pool.MonitorIDs {
			_, err = pools.DisassociateMonitor(lb.network, pool.ID, monId).Extract()
			if err != nil {
				return err
			}

			err = monitors.Delete(lb.network, monId).ExtractErr()
			if err != nil && !isNotFound(err) {
				return err
			}
		}
		for _, memberId := range pool.MemberIDs {
			err = members.Delete(lb.network, memberId).ExtractErr()
			if err != nil && !isNotFound(err) {
				return err
			}
		}
		err = pools.Delete(lb.network, pool.ID).ExtractErr()
		if err != nil && !isNotFound(err) {
			return err
		}
	}

	return nil
}
