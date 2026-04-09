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
	"context"
	"errors"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/baremetal/v1/nodes"
)

var (
	ErrNodeNotAvailable = errors.New("node is not available for provisioning (must be 'available' or 'manageable')")
	ErrValidationFailed = errors.New("node validation failed")
)

// ProvisionWithISO provisions with a live ISO. When the node deploy_interface is not ramdisk,
// sets instance_info.deploy_interface=ramdisk and instance_info.boot_iso=isoURL.
func (c *Client) ProvisionWithISO(ctx context.Context, nodeID, isoURL string) error {
	node, err := c.GetNode(ctx, nodeID)
	if err != nil {
		return err
	}

	ps := nodes.ProvisionState(node.ProvisionState)
	switch ps {
	case nodes.Manageable:
		if err := c.ChangeProvisionState(ctx, nodeID, nodes.ProvisionStateOpts{Target: nodes.TargetProvide}); err != nil {
			return fmt.Errorf("move node to available: %w", err)
		}
		return nil
	case nodes.CleanWait, nodes.Cleaning, nodes.DeployWait, nodes.Deploying:
		return nil
	case nodes.Available:
	default:
		return fmt.Errorf("%w: current state %q", ErrNodeNotAvailable, node.ProvisionState)
	}

	patch := LiveISOUpdateOpts(node, isoURL)
	if len(patch) > 0 {
		if _, err = c.UpdateNode(ctx, nodeID, patch); err != nil {
			return fmt.Errorf("update node instance_info: %w", err)
		}
	}

	validation, err := c.ValidateNode(ctx, nodeID)
	if err != nil {
		return fmt.Errorf("validate node: %w", err)
	}
	if validation != nil {
		if !validation.Boot.Result {
			return fmt.Errorf("%w: boot: %s", ErrValidationFailed, validation.Boot.Reason)
		}
		if !validation.Deploy.Result {
			return fmt.Errorf("%w: deploy: %s", ErrValidationFailed, validation.Deploy.Reason)
		}
	}

	if err := c.ChangeProvisionState(ctx, nodeID, nodes.ProvisionStateOpts{
		Target: nodes.TargetActive,
	}); err != nil {
		return fmt.Errorf("start deploy: %w", err)
	}
	return nil
}

// IsProvisioning returns true when the node is in a transitional state (deploy/clean/delete in progress).
// Do not change power or other state while true — Ironic locks the node and returns 409.
func IsProvisioning(node *nodes.Node) bool {
	ps := nodes.ProvisionState(node.ProvisionState)
	return ps == nodes.CleanWait || ps == nodes.Cleaning ||
		ps == nodes.DeployWait || ps == nodes.Deploying || ps == nodes.Deleting
}

func IsProvisioned(node *nodes.Node) bool {
	return nodes.ProvisionState(node.ProvisionState) == nodes.Active
}

func IsDeployFailed(node *nodes.Node) bool {
	return nodes.ProvisionState(node.ProvisionState) == nodes.DeployFail
}

// CanDeprovision returns true if the node is in a state where TargetDeleted is valid (active or failed).
func CanDeprovision(node *nodes.Node) bool {
	ps := nodes.ProvisionState(node.ProvisionState)
	return ps == nodes.Active || ps == nodes.DeployFail || ps == nodes.CleanFail
}
