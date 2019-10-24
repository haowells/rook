/*
Copyright 2016 The Rook Authors. All rights reserved.

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

package ceph

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"k8s.io/client-go/kubernetes"

	"github.com/rook/rook/cmd/rook/rook"
	"github.com/rook/rook/pkg/daemon/ceph/client"
	osddaemon "github.com/rook/rook/pkg/daemon/ceph/osd"
	"github.com/rook/rook/pkg/operator/ceph/cluster"
	"github.com/rook/rook/pkg/operator/ceph/cluster/mon"
	oposd "github.com/rook/rook/pkg/operator/ceph/cluster/osd"
	osdcfg "github.com/rook/rook/pkg/operator/ceph/cluster/osd/config"
	"github.com/rook/rook/pkg/operator/k8sutil"
	"github.com/rook/rook/pkg/util/flags"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var osdCmd = &cobra.Command{
	Use:   "osd",
	Short: "Provisions and runs the osd daemon",
}
var osdConfigCmd = &cobra.Command{
	Use:   "init",
	Short: "Updates ceph.conf for the osd",
}
var provisionCmd = &cobra.Command{
	Use:   "provision",
	Short: "Generates osd config and prepares an osd for runtime",
}
var filestoreDeviceCmd = &cobra.Command{
	Use:   "filestore-device",
	Short: "Runs the ceph daemon for a filestore device",
}
var osdStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Starts the osd daemon", // OSDs that were provisioned by ceph-volume
}

var (
	osdDataDeviceFilter string
	ownerRefID          string
	mountSourcePath     string
	mountPath           string
	osdID               int
	copyBinariesPath    string
	osdStoreType        string
	osdStringID         string
	osdUUID             string
	osdIsDevice         bool
	pvcBackedOSD        bool
	lvPath              string
)

func addOSDFlags(command *cobra.Command) {
	addOSDConfigFlags(osdConfigCmd)
	addOSDConfigFlags(provisionCmd)

	// flags specific to provisioning
	provisionCmd.Flags().StringVar(&cfg.devices, "data-devices", "", "comma separated list of devices to use for storage")
	provisionCmd.Flags().StringVar(&osdDataDeviceFilter, "data-device-filter", "", "a regex filter for the device names to use, or \"all\"")
	provisionCmd.Flags().StringVar(&cfg.directories, "data-directories", "", "comma separated list of directory paths to use for storage")
	provisionCmd.Flags().StringVar(&cfg.metadataDevice, "metadata-device", "", "device to use for metadata (e.g. a high performance SSD/NVMe device)")
	provisionCmd.Flags().BoolVar(&cfg.forceFormat, "force-format", false,
		"true to force the format of any specified devices, even if they already have a filesystem.  BE CAREFUL!")
	provisionCmd.Flags().BoolVar(&cfg.pvcBacked, "pvc-backed-osd", false, "true to specify a block mode pvc is backing the OSD")
	// flags for generating the osd config
	osdConfigCmd.Flags().IntVar(&osdID, "osd-id", -1, "osd id for which to generate config")
	osdConfigCmd.Flags().BoolVar(&osdIsDevice, "is-device", false, "whether the osd is a device")

	// flags for running filestore on a device
	filestoreDeviceCmd.Flags().StringVar(&mountSourcePath, "source-path", "", "the source path of the device to mount")
	filestoreDeviceCmd.Flags().StringVar(&mountPath, "mount-path", "", "the path where the device should be mounted")

	// flags for running osds that were provisioned by ceph-volume
	osdStartCmd.Flags().StringVar(&osdStringID, "osd-id", "", "the osd ID")
	osdStartCmd.Flags().StringVar(&osdUUID, "osd-uuid", "", "the osd UUID")
	osdStartCmd.Flags().StringVar(&osdStoreType, "osd-store-type", "", "whether the osd is bluestore or filestore")
	osdStartCmd.Flags().BoolVar(&pvcBackedOSD, "pvc-backed-osd", false, "Whether the OSD backing store in PVC or not")
	osdStartCmd.Flags().StringVar(&lvPath, "lv-path", "", "LV path for the OSD created by ceph volume")

	// add the subcommands to the parent osd command
	osdCmd.AddCommand(osdConfigCmd,
		provisionCmd,
		filestoreDeviceCmd,
		osdStartCmd)
}

func addOSDConfigFlags(command *cobra.Command) {
	command.Flags().StringVar(&ownerRefID, "cluster-id", "", "the UID of the cluster CRD that owns this cluster")
	command.Flags().StringVar(&cfg.location, "location", "", "location of this node for CRUSH placement")
	command.Flags().StringVar(&cfg.nodeName, "node-name", os.Getenv("HOSTNAME"), "the host name of the node")

	// OSD store config flags
	command.Flags().IntVar(&cfg.storeConfig.WalSizeMB, "osd-wal-size", osdcfg.WalDefaultSizeMB, "default size (MB) for OSD write ahead log (WAL) (bluestore)")
	command.Flags().IntVar(&cfg.storeConfig.DatabaseSizeMB, "osd-database-size", 0, "default size (MB) for OSD database (bluestore)")
	command.Flags().IntVar(&cfg.storeConfig.JournalSizeMB, "osd-journal-size", osdcfg.JournalDefaultSizeMB, "default size (MB) for OSD journal (filestore)")
	command.Flags().StringVar(&cfg.storeConfig.StoreType, "osd-store", "", "type of backing OSD store to use (bluestore or filestore)")
	command.Flags().IntVar(&cfg.storeConfig.OSDsPerDevice, "osds-per-device", 1, "the number of OSDs per device")
	command.Flags().BoolVar(&cfg.storeConfig.EncryptedDevice, "encrypted-device", false, "whether to encrypt the OSD with dmcrypt")
}

func init() {
	addOSDFlags(osdCmd)
	addCephFlags(osdCmd)
	flags.SetFlagsFromEnv(osdCmd.Flags(), rook.RookEnvVarPrefix)
	flags.SetFlagsFromEnv(osdConfigCmd.Flags(), rook.RookEnvVarPrefix)
	flags.SetFlagsFromEnv(provisionCmd.Flags(), rook.RookEnvVarPrefix)
	flags.SetFlagsFromEnv(filestoreDeviceCmd.Flags(), rook.RookEnvVarPrefix)
	flags.SetFlagsFromEnv(osdStartCmd.Flags(), rook.RookEnvVarPrefix)

	osdConfigCmd.RunE = writeOSDConfig
	provisionCmd.RunE = prepareOSD
	filestoreDeviceCmd.RunE = runFilestoreDeviceOSD
	osdStartCmd.RunE = startOSD
}

// Start the osd daemon if provisioned by ceph-volume
func startOSD(cmd *cobra.Command, args []string) error {
	required := []string{"osd-id", "osd-uuid", "osd-store-type"}
	if err := flags.VerifyRequiredFlags(osdStartCmd, required); err != nil {
		return err
	}

	commonOSDInit(osdStartCmd)

	context := createContext()

	crushLocation, err := getLocation(context.Clientset)
	if err != nil {
		rook.TerminateFatal(err)
	}
	args = append(args, fmt.Sprintf("--crush-location=%s", crushLocation))

	// Run OSD start sequence
	err = osddaemon.StartOSD(context, osdStoreType, osdStringID, osdUUID, lvPath, pvcBackedOSD, args)
	if err != nil {
		rook.TerminateFatal(err)
	}
	return nil
}

// Start the osd daemon for filestore running on a device
func runFilestoreDeviceOSD(cmd *cobra.Command, args []string) error {
	required := []string{"source-path", "mount-path"}
	if err := flags.VerifyRequiredFlags(filestoreDeviceCmd, required); err != nil {
		return err
	}

	args = append(args, []string{
		fmt.Sprintf("--public-addr=%s", cfg.NetworkInfo().PublicAddr),
		fmt.Sprintf("--cluster-addr=%s", cfg.NetworkInfo().ClusterAddr),
	}...)

	commonOSDInit(filestoreDeviceCmd)

	context := createContext()
	err := osddaemon.RunFilestoreOnDevice(context, mountSourcePath, mountPath, args)
	if err != nil {
		rook.TerminateFatal(err)
	}
	return nil
}

func verifyConfigFlags(configCmd *cobra.Command) error {
	required := []string{"cluster-id", "node-name"}
	if err := flags.VerifyRequiredFlags(configCmd, required); err != nil {
		return err
	}
	required = []string{"cluster-name", "mon-endpoints", "mon-secret", "admin-secret"}
	if err := flags.VerifyRequiredFlags(osdCmd, required); err != nil {
		return err
	}
	return nil
}

func writeOSDConfig(cmd *cobra.Command, args []string) error {
	if err := verifyConfigFlags(osdConfigCmd); err != nil {
		return err
	}
	if osdID == -1 {
		return fmt.Errorf("osd id not specified")
	}

	context := createContext()
	commonOSDInit(osdConfigCmd)
	crushLocation, err := getLocation(context.Clientset)
	if err != nil {
		rook.TerminateFatal(err)
	}
	kv := k8sutil.NewConfigMapKVStore(clusterInfo.Name, context.Clientset, metav1.OwnerReference{})

	if err := osddaemon.WriteConfigFile(context, &clusterInfo, kv, osdID, osdIsDevice, cfg.storeConfig, cfg.nodeName, crushLocation); err != nil {
		rook.TerminateFatal(fmt.Errorf("failed to write osd config file. %+v", err))
	}

	return nil
}

// Provision a device or directory for an OSD
func prepareOSD(cmd *cobra.Command, args []string) error {
	if err := verifyConfigFlags(provisionCmd); err != nil {
		return err
	}

	if err := verifyRenamedFlags(osdCmd); err != nil {
		return err
	}

	var dataDevices []osddaemon.DesiredDevice
	if osdDataDeviceFilter != "" {
		if cfg.devices != "" {
			return fmt.Errorf("Only one of --data-devices and --data-device-filter can be specified.")
		}

		dataDevices = []osddaemon.DesiredDevice{
			{Name: osdDataDeviceFilter, IsFilter: true, OSDsPerDevice: cfg.storeConfig.OSDsPerDevice},
		}
	} else {
		var err error
		dataDevices, err = parseDevices(cfg.devices)
		if err != nil {
			rook.TerminateFatal(fmt.Errorf("failed to parse device list (%s). %+v", cfg.devices, err))
		}
	}

	context := createContext()
	commonOSDInit(provisionCmd)
	crushLocation, err := getLocation(context.Clientset)
	if err != nil {
		rook.TerminateFatal(err)
	}
	logger.Infof("crush location of osd: %s", crushLocation)

	forceFormat := false
	ownerRef := cluster.ClusterOwnerRef(clusterInfo.Name, ownerRefID)
	kv := k8sutil.NewConfigMapKVStore(clusterInfo.Name, context.Clientset, ownerRef)
	agent := osddaemon.NewAgent(context, dataDevices, cfg.metadataDevice, cfg.directories, forceFormat,
		crushLocation, cfg.storeConfig, &clusterInfo, cfg.nodeName, kv, cfg.pvcBacked)

	err = osddaemon.Provision(context, agent)
	if err != nil {
		// something failed in the OSD orchestration, update the status map with failure details
		status := oposd.OrchestrationStatus{
			Status:       oposd.OrchestrationStatusFailed,
			Message:      err.Error(),
			PvcBackedOSD: cfg.pvcBacked,
		}
		oposd.UpdateNodeStatus(kv, cfg.nodeName, status)

		rook.TerminateFatal(err)
	}

	return nil
}

func commonOSDInit(cmd *cobra.Command) {
	rook.SetLogLevel()
	rook.LogStartupInfo(cmd.Flags())

	clusterInfo.Monitors = mon.ParseMonEndpoints(cfg.monEndpoints)
}

func getLocation(clientset kubernetes.Interface) (string, error) {
	// Format the basic location properties, then detect the node labels below
	locArgs, err := client.FormatLocation("", cfg.nodeName)
	if err != nil {
		return "", fmt.Errorf("invalid location. %+v", err)
	}

	// use zone/region/hostname labels in the crushmap
	nodeName := os.Getenv(k8sutil.NodeNameEnvVar)
	node, err := getNode(clientset, nodeName)
	if err != nil {
		return "", fmt.Errorf("topologyAware is set, but could not get the node: %+v", err)
	}
	nodeLabels := node.GetLabels()
	updateLocationWithNodeLabels(&locArgs, nodeLabels)

	loc := strings.Join(locArgs, " ")
	logger.Infof("CRUSH location=%s", loc)
	return loc, nil
}

func updateLocationWithNodeLabels(location *[]string, nodeLabels map[string]string) {
	// get zone
	zone, ok := nodeLabels[corev1.LabelZoneFailureDomain]
	if ok {
		client.UpdateCrushMapValue(location, "zone", client.NormalizeCrushName(zone))
	}
	// get region
	region, ok := nodeLabels[corev1.LabelZoneRegion]
	if ok {
		client.UpdateCrushMapValue(location, "region", client.NormalizeCrushName(region))
	}

	// get the labels corresponding to other levels the CRUSH map such as topology.rook.io/rack
	for label, value := range nodeLabels {
		if strings.HasPrefix(label, k8sutil.TopologyLabelPrefix) {
			keys := strings.Split(label, "/")
			if len(keys) != 2 {
				logger.Warningf("ignored invalid node label %q", label)
				continue
			}
			crushType := keys[1]
			validDomain := false
			for _, domain := range oposd.CRUSHTopologyLabels {
				if domain == crushType {
					validDomain = true
					client.UpdateCrushMapValue(location, crushType, client.NormalizeCrushName(value))
					break
				}
			}
			if !validDomain {
				logger.Warningf("ignored invalid crush type %q in label %q", crushType, label)
			}
		}
	}
}

// getNode will try to get the node object for the provided nodeName
// it will try using the node's name it's hostname label
func getNode(clientset kubernetes.Interface, nodeName string) (*corev1.Node, error) {
	var node *corev1.Node
	var err error
	// try to find by the node by matching the provided nodeName
	node, err = clientset.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		listOpts := metav1.ListOptions{LabelSelector: fmt.Sprintf("%s=%s", corev1.LabelHostname, nodeName)}
		nodeList, err := clientset.CoreV1().Nodes().List(listOpts)
		if err != nil || len(nodeList.Items) < 1 {
			return nil, fmt.Errorf("could not find node '%s' hostname label: %+v", nodeName, err)
		}
		return &nodeList.Items[0], nil
	} else if err != nil {
		return nil, fmt.Errorf("could not find node '%s' by name: %+v", nodeName, err)
	}

	return node, nil
}

// Parse the devices, which are comma separated. A colon indicates a non-default number of osds per device
// or a non collocated metadata device.
// For example, one osd will be created on each of sda and sdb, with 5 osds on the nvme01 device.
//   sda:1:::,sdb:1:::,nvme01:5:::
// For example, 3 osds will use sdb SSD for db and 3 osds will use sdc SSD for db.
//   sdd:1:::sdb,sde:1:::sdb,sdf:1:::sdb,sdg:1:::sdc,sdh:1:::sdc,sdi:1:::sdc
func parseDevices(devices string) ([]osddaemon.DesiredDevice, error) {
	var result []osddaemon.DesiredDevice
	parsed := strings.Split(devices, ",")
	for _, device := range parsed {
		parts := strings.Split(device, ":")
		d := osddaemon.DesiredDevice{Name: parts[0], OSDsPerDevice: 1}
		if len(parts) > 1 {
			count, err := strconv.Atoi(parts[1])
			if err != nil {
				return nil, fmt.Errorf("error parsing count from devices (%s). %+v", devices, err)
			}
			if count < 1 {
				return nil, fmt.Errorf("osds per device should be greater than 0 (%s)", parts[1])
			}
			d.OSDsPerDevice = count
		}
		if len(parts) > 2 && parts[2] != "" {
			size, err := strconv.Atoi(parts[2])
			if err != nil {
				return nil, fmt.Errorf("error DatabaseSizeMB (%s) to int. %+v", parts[2], err)
			}
			d.DatabaseSizeMB = size
		}
		if len(parts) > 3 && parts[3] != "" {
			d.DeviceClass = parts[3]
		}
		if len(parts) > 4 {
			d.MetadataDevice = parts[4]
		}
		result = append(result, d)
	}

	logger.Infof("desired devices to configure osds: %+v", result)
	return result, nil
}
