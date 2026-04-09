/*
Copyright 2026.

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

package ironic

import (
	"strings"

	"github.com/gophercloud/gophercloud/v2/openstack/baremetal/v1/nodes"
)

// LiveISOUpdateOpts builds a patch for live-ISO provisioning.
// When the node's deploy_interface is not ramdisk, sets instance_info.deploy_interface=ramdisk
// and instance_info.boot_iso=isoURL so the node uses ramdisk for this deploy.
// When already ramdisk, sets only instance_info.boot_iso.
// Clears image_source and image_checksum from instance_info.
func LiveISOUpdateOpts(current *nodes.Node, isoURL string) nodes.UpdateOpts {
	ramdisk := isRamdisk(current)
	desired := map[string]any{
		"boot_iso":       isoURL,
		"image_source":   nil,
		"image_checksum": nil,
	}
	if !ramdisk {
		desired["deploy_interface"] = "ramdisk"
	}
	return instanceInfoPatch(current, desired)
}

func isRamdisk(node *nodes.Node) bool {
	if node == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(node.DeployInterface), "ramdisk")
}

func instanceInfoPatch(current *nodes.Node, desired map[string]any) nodes.UpdateOpts {
	var out nodes.UpdateOpts
	existing := map[string]any{}
	if current != nil && current.InstanceInfo != nil {
		for k, v := range current.InstanceInfo {
			existing[k] = v
		}
	}
	for k, v := range desired {
		if v == nil {
			if _, ok := existing[k]; ok {
				out = append(out, nodes.UpdateOperation{Op: nodes.RemoveOp, Path: "/instance_info/" + k})
			}
			continue
		}
		path := "/instance_info/" + k
		if _, ok := existing[k]; ok {
			out = append(out, nodes.UpdateOperation{Op: nodes.ReplaceOp, Path: path, Value: v})
		} else {
			out = append(out, nodes.UpdateOperation{Op: nodes.AddOp, Path: path, Value: v})
		}
	}
	return out
}
