/*
Copyright 2023.

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

package deployment

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"

	yaml "gopkg.in/yaml.v3"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	dataplanev1 "github.com/openstack-k8s-operators/dataplane-operator/api/v1beta1"
	infranetworkv1 "github.com/openstack-k8s-operators/infra-operator/apis/network/v1beta1"
	"github.com/openstack-k8s-operators/lib-common/modules/ansible"
	"github.com/openstack-k8s-operators/lib-common/modules/common/helper"
	"github.com/openstack-k8s-operators/lib-common/modules/common/secret"
	utils "github.com/openstack-k8s-operators/lib-common/modules/common/util"
)

// getAnsibleVarsFrom gets ansible vars from ConfigMap/Secret
func getAnsibleVarsFrom(ctx context.Context, helper *helper.Helper, namespace string, ansible *dataplanev1.AnsibleOpts) (map[string]string, error) {

	var result = make(map[string]string)

	client := helper.GetClient()

	// AnsibleVars will override AnsibleVarsFrom variables.
	// Process AnsibleVarsFrom first then allow AnsibleVars to replace existing values.
	for _, varFrom := range ansible.AnsibleVarsFrom {
		switch {
		case varFrom.ConfigMapRef != nil:
			cm := varFrom.ConfigMapRef
			optional := cm.Optional != nil && *cm.Optional
			configMap := &v1.ConfigMap{}
			err := client.Get(ctx, types.NamespacedName{Name: cm.Name, Namespace: namespace}, configMap)
			if err != nil {
				if errors.IsNotFound(err) && optional {
					// ignore error when marked optional
					utils.LogErrorForObject(helper, err, "could not get ansible vars, the configMap: "+cm.Name+"is missing", configMap)
					continue
				}
				return result, err
			}

			for k, v := range configMap.Data {
				if len(varFrom.Prefix) > 0 {
					k = varFrom.Prefix + k
				}

				result[k] = v
			}

		case varFrom.SecretRef != nil:
			s := varFrom.SecretRef
			optional := s.Optional != nil && *s.Optional
			secret := &v1.Secret{}
			err := client.Get(ctx, types.NamespacedName{Name: s.Name, Namespace: namespace}, secret)
			if err != nil {
				if errors.IsNotFound(err) && optional {
					// ignore error when marked optional
					utils.LogErrorForObject(helper, err, "could not get ansible vars, the secret: "+s.Name+"is missing", secret)
					continue
				}
				return result, err
			}

			for k, v := range secret.Data {
				if len(varFrom.Prefix) > 0 {
					k = varFrom.Prefix + k
				}
				result[k] = string(v)
			}
		}
	}

	return result, nil
}

// GenerateNodeSetInventory yields a parsed Inventory for role
func GenerateNodeSetInventory(ctx context.Context, helper *helper.Helper,
	instance *dataplanev1.OpenStackDataPlaneNodeSet,
	allIPSets map[string]infranetworkv1.IPSet, dnsAddresses []string, defaultImages dataplanev1.DataplaneAnsibleImageDefaults) (string, error) {
	inventory := ansible.MakeInventory()
	nodeSetGroup := inventory.AddGroup(instance.Name)
	groupVars, err := getAnsibleVarsFrom(ctx, helper, instance.Namespace, &instance.Spec.NodeTemplate.Ansible)
	if err != nil {
		utils.LogErrorForObject(helper, err, "could not get ansible group vars from configMap/secret", instance)
		return "", err
	}
	for k, v := range groupVars {
		nodeSetGroup.Vars[k] = v
	}
	err = resolveGroupAnsibleVars(&instance.Spec.NodeTemplate, &nodeSetGroup, defaultImages)
	if err != nil {
		utils.LogErrorForObject(helper, err, "Could not resolve ansible group vars", instance)
		return "", err
	}

	// add the NodeSet name variable
	nodeSetGroup.Vars["edpm_nodeset_name"] = instance.Name

	// add TLS ansible variable
	nodeSetGroup.Vars["edpm_tls_certs_enabled"] = instance.Spec.TLSEnabled
	if instance.Spec.Tags != nil {
		nodeSetGroup.Vars["nodeset_tags"] = instance.Spec.Tags
	}

	nodeSetGroup.Vars["ansible_ssh_private_key_file"] = fmt.Sprintf("/runner/env/ssh_key/ssh_key_%s", instance.Name)

	for _, node := range instance.Spec.Nodes {
		host := nodeSetGroup.AddHost(strings.Split(node.HostName, ".")[0])
		hostVars, err := getAnsibleVarsFrom(ctx, helper, instance.Namespace, &node.Ansible)
		if err != nil {
			utils.LogErrorForObject(helper, err, "could not get ansible host vars from configMap/secret", instance)
			return "", err
		}
		for k, v := range hostVars {
			host.Vars[k] = v
		}
		// Use ansible_host if provided else use hostname. Fall back to
		// nodeName if all else fails.
		if node.Ansible.AnsibleHost != "" {
			host.Vars["ansible_host"] = node.Ansible.AnsibleHost
		} else {
			host.Vars["ansible_host"] = node.HostName
		}

		err = resolveHostAnsibleVars(&node, &host)
		if err != nil {
			utils.LogErrorForObject(helper, err, "Could not resolve ansible host vars", instance)
			return "", err
		}

		ipSet, ok := allIPSets[node.HostName]
		if ok {
			populateInventoryFromIPAM(&ipSet, host, dnsAddresses, node.HostName)
		}

	}

	invData, err := inventory.MarshalYAML()
	if err != nil {
		utils.LogErrorForObject(helper, err, "Could not parse NodeSet inventory", instance)
		return "", err
	}
	secretData := map[string]string{
		"inventory": string(invData),
	}
	secretName := fmt.Sprintf("dataplanenodeset-%s", instance.Name)
	template := []utils.Template{
		// Secret
		{
			Name:         secretName,
			Namespace:    instance.Namespace,
			Type:         utils.TemplateTypeNone,
			InstanceType: instance.Kind,
			CustomData:   secretData,
			Labels:       instance.ObjectMeta.Labels,
		},
	}
	err = secret.EnsureSecrets(ctx, helper, instance, template, nil)
	return secretName, err
}

// populateInventoryFromIPAM populates inventory from IPAM
func populateInventoryFromIPAM(
	ipSet *infranetworkv1.IPSet, host ansible.Host,
	dnsAddresses []string, hostName string) {
	var dnsSearchDomains []string
	for _, res := range ipSet.Status.Reservation {
		// Build the vars for ips/routes etc
		entry := strings.ToLower(string(res.Network))
		host.Vars[entry+"_ip"] = res.Address
		_, ipnet, err := net.ParseCIDR(res.Cidr)
		if err == nil {
			netCidr, _ := ipnet.Mask.Size()
			host.Vars[entry+"_cidr"] = netCidr
		}
		if res.Vlan != nil || entry != CtlPlaneNetwork {
			host.Vars[entry+"_vlan_id"] = res.Vlan
		}
		host.Vars[entry+"_mtu"] = res.MTU
		host.Vars[entry+"_gateway_ip"] = res.Gateway
		host.Vars[entry+"_host_routes"] = res.Routes

		if entry == CtlPlaneNetwork {
			host.Vars[entry+"_dns_nameservers"] = dnsAddresses
			if dataplanev1.NodeHostNameIsFQDN(hostName) {
				host.Vars["canonical_hostname"] = hostName
				domain := strings.SplitN("hostName", ".", 2)[1]
				if domain != res.DNSDomain {
					dnsSearchDomains = append(dnsSearchDomains, domain)
				}
			} else {
				host.Vars["canonical_hostname"] = strings.Join([]string{hostName, res.DNSDomain}, ".")
			}
		}
		dnsSearchDomains = append(dnsSearchDomains, res.DNSDomain)
	}
	host.Vars["dns_search_domains"] = dnsSearchDomains
}

// set group ansible vars from NodeTemplate
func resolveGroupAnsibleVars(template *dataplanev1.NodeTemplate, group *ansible.Group,
	defaultImages dataplanev1.DataplaneAnsibleImageDefaults) error {

	if template.Ansible.AnsibleUser != "" {
		group.Vars["ansible_user"] = template.Ansible.AnsibleUser
	}
	if template.Ansible.AnsiblePort > 0 {
		group.Vars["ansible_port"] = strconv.Itoa(template.Ansible.AnsiblePort)
	}
	if template.ManagementNetwork != "" {
		group.Vars["management_network"] = template.ManagementNetwork
	}

	// Set default Service Image Variables in they are not provided by the user.
	// This uses the default values provided by dataplanev1.DataplaneAnsibleImageDefaults
	if template.Ansible.AnsibleVars["edpm_frr_image"] == nil {
		group.Vars["edpm_frr_image"] = defaultImages.Frr
	}
	if template.Ansible.AnsibleVars["edpm_iscsid_image"] == nil {
		group.Vars["edpm_iscsid_image"] = defaultImages.IscsiD
	}
	if template.Ansible.AnsibleVars["edpm_logrotate_crond_image"] == nil {
		group.Vars["edpm_logrotate_crond_image"] = defaultImages.Logrotate
	}
	if template.Ansible.AnsibleVars["edpm_multipathd_image"] == nil {
		group.Vars["edpm_multipathd_image"] = defaultImages.Multipathd
	}
	if template.Ansible.AnsibleVars["edpm_neutron_metadata_agent_image"] == nil {
		group.Vars["edpm_neutron_metadata_agent_image"] = defaultImages.NeutronMetadataAgent
	}
	if template.Ansible.AnsibleVars["edpm_neutron_sriov_agent_image"] == nil {
		group.Vars["edpm_neutron_sriov_image"] = defaultImages.NeutronSRIOVAgent
	}
	if template.Ansible.AnsibleVars["edpm_nova_compute_image"] == nil {
		group.Vars["edpm_nova_compute_image"] = defaultImages.NovaCompute
	}
	if template.Ansible.AnsibleVars["edpm_ovn_controller_agent_image"] == nil {
		group.Vars["edpm_ovn_controller_agent_image"] = defaultImages.OvnControllerAgent
	}
	if template.Ansible.AnsibleVars["edpm_ovn_bgp_agent_image"] == nil {
		group.Vars["edpm_ovn_bgp_agent_image"] = defaultImages.OvnBgpAgent
	}
	if template.Ansible.AnsibleVars["edpm_telemetry_ceilometer_compute_image"] == nil {
		group.Vars["edpm_telemetry_ceilometer_compute_image"] = defaultImages.TelemetryCeilometerCompute
	}
	if template.Ansible.AnsibleVars["edpm_telemetry_ceilometer_ipmi_image"] == nil {
		group.Vars["edpm_telemetry_ceilometer_ipmi_image"] = defaultImages.TelemetryCeilometerIpmi
	}
	if template.Ansible.AnsibleVars["edpm_telemetry_node_exporter_image"] == nil {
		group.Vars["edpm_telemetry_node_exporter_image"] = defaultImages.TelemetryNodeExporter
	}

	err := unmarshalAnsibleVars(template.Ansible.AnsibleVars, group.Vars)
	if err != nil {
		return err
	}
	if len(template.Networks) != 0 {
		nets, netsLower := buildNetworkVars(template.Networks)
		group.Vars["nodeset_networks"] = nets
		group.Vars["networks_lower"] = netsLower
	}

	return nil
}

// set host ansible vars from NodeSection
func resolveHostAnsibleVars(node *dataplanev1.NodeSection, host *ansible.Host) error {

	if node.Ansible.AnsibleUser != "" {
		host.Vars["ansible_user"] = node.Ansible.AnsibleUser
	}
	if node.Ansible.AnsiblePort > 0 {
		host.Vars["ansible_port"] = strconv.Itoa(node.Ansible.AnsiblePort)
	}
	if node.ManagementNetwork != "" {
		host.Vars["management_network"] = node.ManagementNetwork
	}

	err := unmarshalAnsibleVars(node.Ansible.AnsibleVars, host.Vars)
	if err != nil {
		return err
	}
	if len(node.Networks) != 0 {
		nets, netsLower := buildNetworkVars(node.Networks)
		host.Vars["nodeset_networks"] = nets
		host.Vars["networks_lower"] = netsLower
	}
	return nil

}

// unmarshal raw strings into an ansible vars dictionary
func unmarshalAnsibleVars(ansibleVars map[string]json.RawMessage,
	parsedVars map[string]interface{}) error {

	for key, val := range ansibleVars {
		var v interface{}
		err := yaml.Unmarshal(val, &v)
		if err != nil {
			return err
		}
		parsedVars[key] = v
	}
	return nil
}

func buildNetworkVars(networks []infranetworkv1.IPSetNetwork) ([]string, map[string]string) {
	netsLower := make(map[string]string)
	var nets []string
	for _, network := range networks {
		netName := string(network.Name)
		if strings.EqualFold(netName, CtlPlaneNetwork) {
			continue
		}
		nets = append(nets, netName)
		netsLower[netName] = strings.ToLower(netName)
	}
	return nets, netsLower
}
