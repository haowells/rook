/*
Copyright 2019 The Rook Authors. All rights reserved.

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

package nfs

import (
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/pkg/errors"
	rookclient "github.com/rook/rook/pkg/client/clientset/versioned"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/pkg/apis/core/v1/helper"
	"sigs.k8s.io/sig-storage-lib-external-provisioner/controller"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	nfsServerNameSCParam      = "nfsServerName"
	nfsServerNamespaceSCParam = "nfsServerNamespace"
	exportNameSCParam         = "exportName"
)

var (
	mountPath = "/"
)

type Provisioner struct {
	client     kubernetes.Interface
	rookClient rookclient.Interface
}

var _ controller.Provisioner = &Provisioner{}

// NewNFSProvisioner returns an instance of nfsProvisioner
func NewNFSProvisioner(clientset kubernetes.Interface, rookClientset rookclient.Interface) *Provisioner {
	return &Provisioner{clientset, rookClientset}
}

func (p *Provisioner) Provision(options controller.ProvisionOptions) (*v1.PersistentVolume, error) {
	logger.Infof("nfs provisioner: ProvisionOptions %v", options)

	if options.PVC.Spec.Selector != nil {
		return nil, fmt.Errorf("claim Selector is not supported")
	}

	sc, err := p.storageClassForPVC(options.PVC)
	if err != nil {
		return nil, err
	}

	serverName, present := sc.Parameters[nfsServerNameSCParam]
	if !present {
		return nil, errors.Errorf("NFS share Path not found in the storageclass: %v", sc.GetName())
	}

	serverNamespace, present := sc.Parameters[nfsServerNamespaceSCParam]
	if !present {
		return nil, errors.Errorf("NFS share Path not found in the storageclass: %v", sc.GetName())
	}

	exportName, present := sc.Parameters[exportNameSCParam]
	if !present {
		return nil, errors.Errorf("NFS share Path not found in the storageclass: %v", sc.GetName())
	}

	nfsserver, err := p.rookClient.NfsV1alpha1().NFSServers(serverNamespace).Get(serverName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	nfsserversvc, err := p.client.CoreV1().Services(serverNamespace).Get(serverName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	var (
		exportPath string
		found      bool
	)

	for _, export := range nfsserver.Spec.Exports {
		if export.Name == exportName {
			exportPath = path.Join(mountPath, export.PersistentVolumeClaim.ClaimName)
			found = true
		}
	}

	if !found {
		return nil, fmt.Errorf("No export name from storageclass is match with NFSServer %s in namespace %s", nfsserver.Name, nfsserver.Namespace)
	}

	pvName := strings.Join([]string{options.PVC.Namespace, options.PVC.Name, options.PVName}, "-")
	fullPath := path.Join(exportPath, pvName)
	if err := os.MkdirAll(fullPath, 0700); err != nil {
		return nil, errors.New("unable to create directory to provision new pv: " + err.Error())
	}

	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: options.PVName,
		},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: *options.StorageClass.ReclaimPolicy,
			AccessModes:                   options.PVC.Spec.AccessModes,
			MountOptions:                  options.StorageClass.MountOptions,
			Capacity: v1.ResourceList{
				v1.ResourceName(v1.ResourceStorage): options.PVC.Spec.Resources.Requests[v1.ResourceName(v1.ResourceStorage)],
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				NFS: &v1.NFSVolumeSource{
					Server:   nfsserversvc.Spec.ClusterIP,
					Path:     fullPath,
					ReadOnly: false,
				},
			},
		},
	}

	return pv, nil
}

func (p *Provisioner) Delete(volume *v1.PersistentVolume) error {
	nfsPath := volume.Spec.PersistentVolumeSource.NFS.Path
	pvName := path.Base(nfsPath)

	sc, err := p.storageClassForPV(volume)
	if err != nil {
		return err
	}

	serverName, present := sc.Parameters[nfsServerNameSCParam]
	if !present {
		return errors.Errorf("NFS share Path not found in the storageclass: %v", sc.GetName())
	}

	serverNamespace, present := sc.Parameters[nfsServerNamespaceSCParam]
	if !present {
		return errors.Errorf("NFS share Path not found in the storageclass: %v", sc.GetName())
	}

	exportName, present := sc.Parameters[exportNameSCParam]
	if !present {
		return errors.Errorf("NFS share Path not found in the storageclass: %v", sc.GetName())
	}

	nfsserver, err := p.rookClient.NfsV1alpha1().NFSServers(serverNamespace).Get(serverName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	var (
		exportPath string
		found      bool
	)

	for _, export := range nfsserver.Spec.Exports {
		if export.Name == exportName {
			exportPath = path.Join(mountPath, export.PersistentVolumeClaim.ClaimName)
			found = true
		}
	}

	if !found {
		return fmt.Errorf("No export name from storageclass is match with NFSServer %s in namespace %s", nfsserver.Name, nfsserver.Namespace)
	}

	fullPath := path.Join(exportPath, pvName)
	return os.RemoveAll(fullPath)
}

func (p *Provisioner) storageClassForPV(pv *v1.PersistentVolume) (*storagev1.StorageClass, error) {
	if p.client == nil {
		return nil, fmt.Errorf("Cannot get kube client")
	}
	className := helper.GetPersistentVolumeClass(pv)
	if className == "" {
		return nil, fmt.Errorf("Volume has no storage class")
	}

	return p.client.StorageV1().StorageClasses().Get(className, metav1.GetOptions{})
}

func (p *Provisioner) storageClassForPVC(pvc *v1.PersistentVolumeClaim) (*storagev1.StorageClass, error) {
	if p.client == nil {
		return nil, fmt.Errorf("Cannot get kube client")
	}
	className := helper.GetPersistentVolumeClaimClass(pvc)
	if className == "" {
		return nil, fmt.Errorf("Volume has no storage class")
	}

	return p.client.StorageV1().StorageClasses().Get(className, metav1.GetOptions{})
}
