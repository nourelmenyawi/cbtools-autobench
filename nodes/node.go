// Copyright 2021 Couchbase Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//        http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package nodes

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/jamesl33/cbtools-autobench/ssh"
	"github.com/jamesl33/cbtools-autobench/value"

	"github.com/apex/log"
	"github.com/pkg/errors"
)

// Node represents a connection to a remote Couchbase Server node (note that the node may or may not be setup yet).
type Node struct {
	blueprint *value.NodeBlueprint
	client    *ssh.Client
}

// NewNode creates a connection to the remote node using the provided ssh config.
func NewNode(config *value.SSHConfig, blueprint *value.NodeBlueprint) (*Node, error) {
	client, err := ssh.NewClient(blueprint.Host, config)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create ssh client")
	}

	return &Node{blueprint: blueprint, client: client}, nil
}

// provision the node by installing the required dependencies (including Couchbase Server).
func (n *Node) provision(packagePath string) error {
	err := n.installDeps()
	if err != nil {
		return errors.Wrap(err, "failed to install dependencies")
	}

	err = n.uninstallCB()
	if err != nil {
		return errors.Wrap(err, "failed to uninstall Couchbase Server")
	}

	err = n.installCB(packagePath)
	if err != nil {
		return errors.Wrap(err, "failed to install Couchbase Server")
	}

	// We've got to wait for things to complete, for example we need to actually wait for Couchbase Server to start
	time.Sleep(30 * time.Second)

	err = n.giveCBPermissions()
	if err != nil {
		return errors.Wrap(err, "failed to give Couchbase Server permissions")
	}

	return nil
}

// installDeps installs any required platform specific dependencies which are missing on the remote machine.
func (n *Node) installDeps() error {
	log.WithField("host", n.blueprint.Host).Info("Installing dependencies")

	return n.client.InstallPackages(n.client.Platform.Dependencies()...)
}

// uninstallCB will uninstall Couchbase Server from the remote node ensuring a clean slate.
func (n *Node) uninstallCB() error {
	log.WithField("host", n.blueprint.Host).Info("Uninstalling 'couchbase-server'")

	err := n.client.UninstallPackages("couchbase-server")
	if err != nil {
		return errors.Wrap(err, "failed to uninstall 'couchbase-server'")
	}

	log.WithField("host", n.blueprint.Host).Info("Purging install directory")

	err = n.client.RemoveDirectory(value.CBInstallDirectory)
	if err != nil {
		return errors.Wrapf(err, "failed to cleanup install directory at '%s'", value.CBInstallDirectory)
	}

	return nil
}

// installCB uploads the Couchbase Server install package to the remote machine and installs it.
//
// NOTE: The package archive will be removed upon completion.
func (n *Node) installCB(localPath string) error {
	remotePath := filepath.Join("/home/ec2-user", filepath.Base(localPath))

	log.WithField("host", n.blueprint.Host).Info("Uploading package archive")

	err := n.client.SecureUpload(localPath, remotePath)
	switch {
	case err != nil:
		return errors.Wrap(err, "failed to upload package archive")
	case errors.Is(err, fmt.Errorf("File already exists")):
		log.WithField("host", n.blueprint.Host).Info("Package archive already exists")
		return nil
	}

	log.WithField("host", n.blueprint.Host).Info("Installing 'couchbase-server'")

	err = n.client.InstallPackageAt(remotePath)
	if err != nil {
		return errors.Wrap(err, "failed to install 'couchbase-server'")
	}

	log.WithField("host", n.blueprint.Host).Info("Cleaning up package archive")

	err = n.client.RemoveFile(remotePath)
	if err != nil {
		return errors.Wrap(err, "failed to remove package archive")
	}

	return nil
}

// createDataPath ensures that the users chosen data path exists on the remote machine.
func (n *Node) createDataPath() error {
	if n.blueprint.DataPath == "" {
		return nil
	}

	log.WithField("host", n.blueprint.Host).Info("Creating/configuring data path")

	_, err := n.client.ExecuteCommand(value.NewCommand("mkdir -p %s", n.blueprint.DataPath))
	if err != nil {
		return errors.Wrap(err, "failed to create remote data directory")
	}

	_, err = n.client.ExecuteCommand(value.NewCommand("chown -R couchbase:couchbase %s", n.blueprint.DataPath))
	if err != nil {
		return errors.Wrap(err, "failed to chown remote data directory")
	}

	return nil
}

func (n *Node) createIndexPath() error {
	if n.blueprint.IndexPath == "" {
		return nil
	}

	log.WithField("host", n.blueprint.Host).Info("Creating/configuring index path")

	_, err := n.client.ExecuteCommand(value.NewCommand("mkdir -p %s", n.blueprint.IndexPath))
	if err != nil {
		return errors.Wrap(err, "failed to create remote index directory")
	}

	_, err = n.client.ExecuteCommand(value.NewCommand("chown -R couchbase:couchbase %s", n.blueprint.IndexPath))
	if err != nil {
		return errors.Wrap(err, "failed to chown remote index directory")
	}

	return nil
}

// initializeCB will perform node level initialization of Couchbase Server.
func (n *Node) initializeCB() error {
	fields := log.Fields{
		"host":       n.blueprint.Host,
		"data_path":  n.blueprint.DataPath,
		"index_path": n.blueprint.IndexPath,
	}

	log.WithFields(fields).Info("Initializing node")

	init := "couchbase-cli node-init -c localhost:8091 -u Administrator -p asdasd"
	if n.blueprint.DataPath != "" {
		init += fmt.Sprintf(" --node-init-data-path %s", n.blueprint.DataPath)
	}

	if n.blueprint.IndexPath != "" {
		init += fmt.Sprintf(" --node-init-index-path %s", n.blueprint.IndexPath)
	}

	_, err := n.client.ExecuteCommand(value.NewCommand(init))

	return err
}

// disableCB will disable Couchbase Server on the remote node, this will done on the backup client to free up resources
// for 'cbbackupmgr'.
func (n *Node) disableCB() error {
	log.WithField("host", n.blueprint.Host).Info("Disabling 'couchbase-server'")

	_, err := n.client.ExecuteCommand(n.client.Platform.CommandDisableCouchbase())

	return err
}

// loginAsRoot will attempt to login as the root user on the remote machine.
func (n *Node) loginAsRoot() error {
	// Become root
	_, err := n.client.ExecuteCommand("sudo -s")
	if err != nil {
		log.Fatalf("failed to become root: %v", err)
	}

	// Read the authorized_keys file content
	output, err := n.client.ExecuteCommand("sudo cat /root/.ssh/authorized_keys")
	if err != nil {
		log.Fatalf("failed to read authorized_keys: %v", err)
	}

	// Find the position of the first occurrence of 'ssh-rsa'
	index := strings.Index(string(output), "ssh-rsa")
	if index == -1 {
		log.Fatalf("ssh-rsa not found in authorized_keys")
	}

	// Trim the content to keep only the portion starting from 'ssh-rsa'
	newContent := output[index:]

	// Write the new content back to authorized_keys
	msg := fmt.Sprintf("echo '%s' | sudo tee /root/.ssh/authorized_keys", newContent)
	_, err = n.client.ExecuteCommand(value.Command(msg))
	if err != nil {
		log.Fatalf("failed to write to authorized_keys: %v", err)
	}

	// Restart the SSH service
	_, err = n.client.ExecuteCommand("sudo systemctl restart sshd.service || sudo systemctl restart ssh.service")
	if err != nil {
		log.Fatalf("failed to restart SSH service: %v", err)
	}

	return err
}

// checkAndPartitionEBS will check for an EBS volume, if it exists partition it to a "/mnt" using gdisk command with n, p and w commands then make a mkfs file structure name it /dev/nvme1n1p1 and mount /mnt on it
func (n *Node) checkAndPartitionEBS() error {
	log.WithField("host", n.blueprint.Host).Info("Checking and partitioning EBS volume")

	checkAllVolumes := fmt.Sprintf("lsblk -o NAME,SIZE,TYPE,MOUNTPOINT")
	allVolumes, err := n.client.ExecuteCommand(value.NewCommand(checkAllVolumes))
	if err != nil {
		return fmt.Errorf("failed to check for all volumes: %w", err)
	}

	volumeName, err := ExtractLastVolumeName(string(allVolumes))
	if err != nil {
		return fmt.Errorf("failed to extract last volume name: %w", err)
	}

	log.WithField("host", n.blueprint.Host).Info(string(volumeName))

	// Check if EBS volume exists
	checkVolume := fmt.Sprintf("lsblk | grep %s", volumeName)
	_, err = n.client.ExecuteCommand(value.NewCommand(checkVolume))
	if err != nil {
		return fmt.Errorf("failed to check for EBS volume: %w", err)
	}

	partitionedVolume := fmt.Sprintf("/dev/%sp1", volumeName)
	// Check if EBS volume is already partitioned
	checkPartition := fmt.Sprintf("lsblk /dev/%s | grep %s", volumeName, partitionedVolume)
	log.WithField("host", n.blueprint.Host).Info(checkPartition)
	_, err = n.client.ExecuteCommand(value.NewCommand(checkPartition))
	if err != nil {
		// EBS volume is not partitioned, we can proceed with partitioning
		partitionVolume := fmt.Sprintf("echo ',,,;' | sfdisk /dev/%s", volumeName)
		_, err = n.client.ExecuteCommand(value.NewCommand(partitionVolume))
		if err != nil {
			return fmt.Errorf("failed to partition EBS volume: %w", err)
		}
	} else {
		// EBS volume is already partitioned, we should not proceed with partitioning
		log.WithField("host", n.blueprint.Host).Info("EBS volume is already partitioned, skipping partitioning")
		return nil
	}

	// Make a mkfs file structure
	makeFileStructure := fmt.Sprintf("mkfs.xfs /dev/%sp1", volumeName)
	_, err = n.client.ExecuteCommand(value.NewCommand(makeFileStructure))
	if err != nil {
		return fmt.Errorf("failed to make mkfs file structure: %w", err)
	}

	// Mount /mnt on it
	mountVolume := fmt.Sprintf("mount /dev/%sp1 /mnt", volumeName)
	_, err = n.client.ExecuteCommand(value.NewCommand(mountVolume))
	if err != nil {
		return fmt.Errorf("failed to mount /mnt on EBS volume: %w", err)
	}

	changePermissions := "chmod 777 /mnt"
	_, err = n.client.ExecuteCommand(value.NewCommand(changePermissions))
	if err != nil {
		return fmt.Errorf("failed to change permissions on /mnt: %w", err)
	}

	return nil
}

// giveCBPermissions will give the Couchbase Server user the required
// permissions to access the data path.
func (n *Node) giveCBPermissions() error {
	// Run cmhod +X on EBS volume /mnt
	changePermissions := "sudo chown -R couchbase:couchbase /mnt"
	_, err := n.client.ExecuteCommand(value.NewCommand(changePermissions))
	if err != nil {
		return fmt.Errorf("failed to change permissions on /mnt: %w", err)
	}

	return nil
}

// ExtractLastVolumeName extracts the last volume name from lsblk output
func ExtractLastVolumeName(lsblkOutput string) (string, error) {
	lines := strings.Split(lsblkOutput, "\n")
	var lastVolumeName string

	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) > 2 && fields[2] == "disk" {
			lastVolumeName = fields[0]
		}
	}

	if lastVolumeName == "" {
		return "", fmt.Errorf("no disk volume found in lsblk output")
	}
	return lastVolumeName, nil
}

// Close releases any resources in use by the connection.
func (n *Node) Close() error {
	return n.client.Close()
}
