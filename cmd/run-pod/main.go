// Copyright 2019 Yunion
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

package main

import (
	"context"
	"time"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"

	"yunion.io/x/jsonutils"
	"yunion.io/x/log"

	"yunion.io/x/onecloud/pkg/util/pod"
)

func main() {
	ctl, err := pod.NewCRI("unix:///var/run/onecloud/containerd/containerd.sock", 3*time.Second)
	if err != nil {
		log.Fatalf("NewCRI: %v", err)
	}
	ctx := context.Background()
	imgs, err := ctl.ListImages(ctx, nil)
	if err != nil {
		log.Fatalf("ListImages: %v", err)
	}
	for _, img := range imgs {
		log.Infof("get img: %s", img.String())
	}

	ver, err := ctl.Version(context.Background())
	if err != nil {
		log.Fatalf("get version: %v", err)
	}
	log.Infof("get version: %s", ver.String())

	// create container
	podCfg := &runtimeapi.PodSandboxConfig{
		Metadata: &runtimeapi.PodSandboxMetadata{
			Name:      "test-gpu",
			Uid:       "e25e38ef-fe98-4993-8641-699cd0530fc0",
			Namespace: "27c9464ab54947328a29298761895be3",
			Attempt:   1,
		},
		Hostname:     "test-gpu",
		LogDirectory: "",
		DnsConfig:    nil,
		PortMappings: nil,
		Labels:       nil,
		Annotations:  nil,
		Linux:        nil,
		Windows:      nil,
	}
	ctrCfgs := []*runtimeapi.ContainerConfig{
		{
			Metadata: &runtimeapi.ContainerMetadata{
				Name: "nvidia-smi",
			},
			Image: &runtimeapi.ImageSpec{
				Image: "ubuntu",
			},
			Command: []string{"sleep", "100d"},
			Linux:   &runtimeapi.LinuxContainerConfig{
				//SecurityContext: &runtimeapi.LinuxContainerSecurityContext{
				//	Privileged: true,
				//},
			},
			Envs: []*runtimeapi.KeyValue{
				{
					Key:   "NVIDIA_VISIBLE_DEVICES",
					Value: "GPU-e588f4f5-29a4-4374-a335-86e120b50e14,GPU-f7160578-ba3b-3e42-6991-14c815ce032a,GPU-679b381b-eb98-62b7-c7c4-175f7d751aad",
				},
				{
					Key:   "NVIDIA_DRIVER_CAPABILITIES",
					Value: "compute,utility",
				},
			},
			/*Devices: []*runtimeapi.Device{
				{
					HostPath:      "/dev/nvidia0",
					ContainerPath: "/dev/nvidia0",
					Permissions:   "rwm",
				},
			},*/
		},
	}
	resp, err := ctl.RunContainers(ctx, podCfg, ctrCfgs, "")
	if err != nil {
		log.Fatalf("RunContainers: %v", err)
	}
	log.Infof("RunContainers: %s", jsonutils.Marshal(resp))
}
