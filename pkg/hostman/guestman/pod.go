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

package guestman

import (
	"context"
	"fmt"
	"io/ioutil"
	"path"
	"path/filepath"
	"strings"
	"time"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"

	"yunion.io/x/jsonutils"
	"yunion.io/x/log"
	"yunion.io/x/pkg/errors"
	"yunion.io/x/pkg/util/sets"

	"yunion.io/x/onecloud/pkg/apis"
	computeapi "yunion.io/x/onecloud/pkg/apis/compute"
	hostapi "yunion.io/x/onecloud/pkg/apis/host"
	"yunion.io/x/onecloud/pkg/hostman/container/device"
	"yunion.io/x/onecloud/pkg/hostman/container/lifecycle"
	"yunion.io/x/onecloud/pkg/hostman/container/volume_mount"
	"yunion.io/x/onecloud/pkg/hostman/guestman/desc"
	deployapi "yunion.io/x/onecloud/pkg/hostman/hostdeployer/apis"
	"yunion.io/x/onecloud/pkg/hostman/hostutils"
	"yunion.io/x/onecloud/pkg/hostman/isolated_device"
	"yunion.io/x/onecloud/pkg/hostman/options"
	"yunion.io/x/onecloud/pkg/hostman/storageman"
	"yunion.io/x/onecloud/pkg/httperrors"
	"yunion.io/x/onecloud/pkg/mcclient"
	"yunion.io/x/onecloud/pkg/mcclient/auth"
	computemod "yunion.io/x/onecloud/pkg/mcclient/modules/compute"
	"yunion.io/x/onecloud/pkg/util/fileutils2"
	"yunion.io/x/onecloud/pkg/util/netutils2/getport"
	"yunion.io/x/onecloud/pkg/util/pod"
	"yunion.io/x/onecloud/pkg/util/procutils"
)

type PodInstance interface {
	GuestRuntimeInstance

	CreateContainer(ctx context.Context, userCred mcclient.TokenCredential, id string, input *hostapi.ContainerCreateInput) (jsonutils.JSONObject, error)
	StartContainer(ctx context.Context, userCred mcclient.TokenCredential, ctrId string, input *hostapi.ContainerCreateInput) (jsonutils.JSONObject, error)
	DeleteContainer(ctx context.Context, cred mcclient.TokenCredential, id string) (jsonutils.JSONObject, error)
	SyncContainerStatus(ctx context.Context, cred mcclient.TokenCredential, ctrId string) (jsonutils.JSONObject, error)
	StopContainer(ctx context.Context, userCred mcclient.TokenCredential, ctrId string, body jsonutils.JSONObject) (jsonutils.JSONObject, error)
	PullImage(ctx context.Context, userCred mcclient.TokenCredential, ctrId string, input *hostapi.ContainerPullImageInput) (jsonutils.JSONObject, error)
}

type sContainer struct {
	Id    string `json:"id"`
	Index int    `json:"index"`
	CRIId string `json:"cri_id"`
}

func newContainer(id string) *sContainer {
	return &sContainer{
		Id: id,
	}
}

type sPodGuestInstance struct {
	*sBaseGuestInstance
	containers map[string]*sContainer
}

func newPodGuestInstance(id string, man *SGuestManager) PodInstance {
	return &sPodGuestInstance{
		sBaseGuestInstance: newBaseGuestInstance(id, man, computeapi.HYPERVISOR_POD),
		containers:         make(map[string]*sContainer),
	}
}

func (s *sPodGuestInstance) CleanGuest(ctx context.Context, params interface{}) (jsonutils.JSONObject, error) {
	criId := s.getCRIId()
	if criId != "" {
		if err := s.getCRI().RemovePod(ctx, criId); err != nil {
			return nil, errors.Wrapf(err, "RemovePod with cri_id %q", criId)
		}
	}
	return nil, DeleteHomeDir(s)
}

func (s *sPodGuestInstance) ImportServer(pendingDelete bool) {
	log.Infof("======pod %s ImportServer do nothing", s.Id)
	// TODO: 参考SKVMGuestInstance，可以做更多的事，比如同步状态
	s.manager.SaveServer(s.Id, s)
	s.manager.RemoveCandidateServer(s)
}

func (s *sPodGuestInstance) DeployFs(ctx context.Context, userCred mcclient.TokenCredential, deployInfo *deployapi.DeployInfo) (jsonutils.JSONObject, error) {
	return nil, nil
}

func (s *sPodGuestInstance) IsStopped() bool {
	//TODO implement me
	panic("implement me")
}

func (s *sPodGuestInstance) IsSuspend() bool {
	return false
}

func (s *sPodGuestInstance) getCRI() pod.CRI {
	return s.manager.GetCRI()
}

func (s *sPodGuestInstance) getPod(ctx context.Context) (*runtimeapi.PodSandbox, error) {
	pods, err := s.getCRI().ListPods(ctx, pod.ListPodOptions{})
	if err != nil {
		return nil, errors.Wrap(err, "ListPods")
	}
	for _, p := range pods {
		if p.Metadata.Uid == s.Id {
			return p, nil
		}
	}
	return nil, errors.Wrap(httperrors.ErrNotFound, "Not found pod from containerd")
}

func (s *sPodGuestInstance) IsRunning() bool {
	_, err := s.getPod(context.Background())
	if err != nil {
		log.Warningf("check if pod of guest %s is running", s.Id)
		return false
	}
	return true
	/*ctrs, err := s.getCRI().ListContainers(context.Background(), pod.ListContainerOptions{
		PodId: s.Id,
	})
	if err != nil {
		log.Errorf("List containers of pod %q", s.GetId())
		return false
	}
	// TODO: container s状态应该存在每个 container 资源里面
	// Pod 状态只放 guest 表
	isAllRunning := true
	for _, ctr := range ctrs {
		if ctr.State != runtimeapi.ContainerState_CONTAINER_RUNNING {
			isAllRunning = false
			break
		}
	}
	return isAllRunning*/
}

func (s *sPodGuestInstance) HandleGuestStatus(ctx context.Context, status string, body *jsonutils.JSONDict) (jsonutils.JSONObject, error) {
	body.Set("status", jsonutils.NewString(status))
	hostutils.TaskComplete(ctx, body)
	return nil, nil
}

func (s *sPodGuestInstance) HandleGuestStart(ctx context.Context, userCred mcclient.TokenCredential, body jsonutils.JSONObject) (jsonutils.JSONObject, error) {
	hostutils.DelayTaskWithWorker(ctx, func(ctx context.Context, params interface{}) (jsonutils.JSONObject, error) {
		resp, err := s.startPod(ctx, userCred)
		if err != nil {
			return nil, errors.Wrap(err, "startPod")
		}
		return jsonutils.Marshal(resp), nil
	}, nil, s.manager.GuestStartWorker)
	return nil, nil
}

func (s *sPodGuestInstance) HandleStop(ctx context.Context, timeout int64) error {
	hostutils.DelayTask(ctx, func(ctx context.Context, params interface{}) (jsonutils.JSONObject, error) {
		err := s.stopPod(ctx, timeout)
		if err != nil {
			return nil, errors.Wrap(err, "stopPod")
		}
		return nil, nil
	}, nil)
	return nil
}

func (s *sPodGuestInstance) getCreateParams() (jsonutils.JSONObject, error) {
	createParamsStr, ok := s.GetDesc().Metadata[computeapi.VM_METADATA_CREATE_PARAMS]
	if !ok {
		return nil, errors.Errorf("not found %s in metadata", computeapi.VM_METADATA_CREATE_PARAMS)
	}
	return jsonutils.ParseString(createParamsStr)
}

func (s *sPodGuestInstance) getPodCreateParams() (*computeapi.PodCreateInput, error) {
	createParams, err := s.getCreateParams()
	if err != nil {
		return nil, errors.Wrapf(err, "getCreateParams")
	}
	input := new(computeapi.PodCreateInput)
	if err := createParams.Unmarshal(input, "pod"); err != nil {
		return nil, errors.Wrapf(err, "unmarshal to pod creation input")
	}
	return input, nil
}

func (s *sPodGuestInstance) getPodLogDir() string {
	return filepath.Join(s.HomeDir(), "logs")
}

func (s *sPodGuestInstance) GetDisks() []*desc.SGuestDisk {
	return s.GetDesc().Disks
}

func (s *sPodGuestInstance) mountPodVolumes() error {
	for _, vol := range s.getContainerVolumeMounts() {
		if err := volume_mount.GetDriver(vol.Type).Mount(s, vol); err != nil {
			return errors.Wrapf(err, "mount volume %s", jsonutils.Marshal(vol))
		}
	}
	return nil
}

func (s *sPodGuestInstance) umountPodVolumes() error {
	for _, vol := range s.getContainerVolumeMounts() {
		if err := volume_mount.GetDriver(vol.Type).Unmount(s, vol); err != nil {
			return errors.Wrapf(err, "Unmount volume %s", jsonutils.Marshal(vol))
		}
	}
	return nil
}

func (s *sPodGuestInstance) getContainerVolumeMounts() []*apis.ContainerVolumeMount {
	mnts := make([]*apis.ContainerVolumeMount, 0)
	for _, ctr := range s.GetDesc().Containers {
		for _, vol := range ctr.Spec.VolumeMounts {
			tmp := vol
			mnts = append(mnts, tmp)
		}
	}
	return mnts
}

func (s *sPodGuestInstance) GetVolumesDir() string {
	return filepath.Join(s.HomeDir(), "volumes")
}

func (s *sPodGuestInstance) GetDiskMountPoint(disk storageman.IDisk) string {
	return filepath.Join(s.GetVolumesDir(), disk.GetId())
}

func (s *sPodGuestInstance) getPodPrivilegedMode(input *computeapi.PodCreateInput) bool {
	for _, ctr := range input.Containers {
		if ctr.Privileged {
			return true
		}
	}
	return false
}

func (s *sPodGuestInstance) getPortMappings(input []*computeapi.PodPortMapping) ([]*runtimeapi.PortMapping, error) {
	result := make([]*runtimeapi.PortMapping, len(input))
	for idx := range input {
		pm, err := s.getPortMapping(input[idx])
		if err != nil {
			return nil, errors.Wrapf(err, "get port mapping %s", jsonutils.Marshal(input[idx]))
		}
		result[idx] = pm
	}
	return result, nil
}

func (s *sPodGuestInstance) getOtherPods() []*sPodGuestInstance {
	man := s.manager
	otherPods := make([]*sPodGuestInstance, 0)
	man.Servers.Range(func(id, value any) bool {
		if id == s.Id {
			return true
		}
		ins := value.(GuestRuntimeInstance)
		pod, ok := ins.(*sPodGuestInstance)
		if !ok {
			return true
		}
		otherPods = append(otherPods, pod)
		return true
	})
	return otherPods
}

func (s *sPodGuestInstance) getOtherPodsUsedPorts() (map[computeapi.PodPortMappingProtocol]sets.Int, error) {
	otherPods := s.getOtherPods()
	ret := make(map[computeapi.PodPortMappingProtocol]sets.Int)
	for _, pod := range otherPods {
		pms, err := pod.GetPodMetadataPortMappings()
		if err != nil {
			return nil, errors.Wrapf(err, "get pod %s port_mappins", pod.GetId())
		}
		for _, pm := range pms {
			ps, ok := ret[pm.Protocol]
			if !ok {
				ps = sets.NewInt()
			}
			ps.Insert(int(pm.HostPort))
			ret[pm.Protocol] = ps
		}
	}
	return ret, nil
}

func (s *sPodGuestInstance) getPortMapping(pm *computeapi.PodPortMapping) (*runtimeapi.PortMapping, error) {
	runtimePm := &runtimeapi.PortMapping{
		ContainerPort: int32(pm.ContainerPort),
		HostIp:        pm.HostIp,
	}
	portProtocol := getport.TCP
	switch pm.Protocol {
	case computeapi.PodPortMappingProtocolTCP:
		runtimePm.Protocol = runtimeapi.Protocol_TCP
		portProtocol = getport.TCP
	case computeapi.PodPortMappingProtocolUDP:
		runtimePm.Protocol = runtimeapi.Protocol_UDP
		portProtocol = getport.UDP
	//case computeapi.PodPortMappingProtocolSCTP:
	//	runtimePm.Protocol = runtimeapi.Protocol_SCTP
	default:
		return nil, errors.Errorf("invalid protocol: %q", pm.Protocol)
	}
	// listen random port
	otherPorts, err := s.getOtherPodsUsedPorts()
	if err != nil {
		return nil, errors.Wrap(err, "getOtherPodsUsedPorts")
	}
	if pm.HostPort != nil {
		runtimePm.HostPort = int32(*pm.HostPort)
		if getport.IsPortUsed(portProtocol, "", *pm.HostPort) {
			return nil, httperrors.NewInputParameterError("host_port %d is used", *pm.HostPort)
		}
		usedPorts, ok := otherPorts[pm.Protocol]
		if ok {
			if usedPorts.Has(*pm.HostPort) {
				return nil, errors.Wrapf(err, "%s host_port %d is already used", pm.Protocol, *pm.HostPort)
			}
		}
		return runtimePm, nil
	} else {
		start := 20000
		end := 25000
		if pm.HostPortRange != nil {
			start = pm.HostPortRange.Start
			end = pm.HostPortRange.End
		}
		otherPodPorts, ok := otherPorts[pm.Protocol]
		if !ok {
			otherPodPorts = sets.NewInt()
		}
		portResult, err := getport.GetPortByRangeBySets(portProtocol, start, end, otherPodPorts)
		if err != nil {
			return nil, errors.Wrapf(err, "listen %s port inside %d and %d", pm.Protocol, start, end)
		}
		runtimePm.HostPort = int32(portResult.Port)
		return runtimePm, nil
	}
}

func (s *sPodGuestInstance) getCgroupParent() string {
	// return fmt.Sprintf("/cloudpods/%s", s.GetId())
	return "/cloudpods"
}

func (s *sPodGuestInstance) startPod(ctx context.Context, userCred mcclient.TokenCredential) (*computeapi.PodStartResponse, error) {
	podInput, err := s.getPodCreateParams()
	if err != nil {
		return nil, errors.Wrap(err, "getPodCreateParams")
	}
	if err := s.mountPodVolumes(); err != nil {
		return nil, errors.Wrap(err, "mountPodVolumes")
	}
	podCfg := &runtimeapi.PodSandboxConfig{
		Metadata: &runtimeapi.PodSandboxMetadata{
			Name:      s.GetDesc().Name,
			Uid:       s.GetId(),
			Namespace: s.GetDesc().TenantId,
			Attempt:   1,
		},
		Hostname:     s.GetDesc().Hostname,
		LogDirectory: s.getPodLogDir(),
		DnsConfig:    nil,
		PortMappings: nil,
		Labels:       nil,
		Annotations:  nil,
		Linux: &runtimeapi.LinuxPodSandboxConfig{
			CgroupParent: s.getCgroupParent(),
			SecurityContext: &runtimeapi.LinuxSandboxSecurityContext{
				NamespaceOptions:   nil,
				SelinuxOptions:     nil,
				RunAsUser:          nil,
				RunAsGroup:         nil,
				ReadonlyRootfs:     false,
				SupplementalGroups: nil,
				Privileged:         s.getPodPrivilegedMode(podInput),
				Seccomp: &runtimeapi.SecurityProfile{
					ProfileType: runtimeapi.SecurityProfile_Unconfined,
				},
				Apparmor: &runtimeapi.SecurityProfile{
					ProfileType: runtimeapi.SecurityProfile_Unconfined,
				},
				SeccompProfilePath: "",
			},
			Sysctls: nil,
		},
		Windows: nil,
	}

	if len(podInput.PortMappings) != 0 {
		pms, err := s.getPortMappings(podInput.PortMappings)
		if err != nil {
			return nil, errors.Wrap(err, "get port mappings")
		}
		podCfg.PortMappings = pms
	}

	criId, err := s.getCRI().RunPod(ctx, podCfg, "")
	if err != nil {
		return nil, errors.Wrap(err, "cri.RunPod")
	}
	if err := s.setCRIInfo(ctx, userCred, criId, podCfg); err != nil {
		return nil, errors.Wrap(err, "setCRIId")
	}
	return &computeapi.PodStartResponse{
		CRIId:     criId,
		IsRunning: false,
	}, nil
}

func (s *sPodGuestInstance) stopPod(ctx context.Context, timeout int64) error {
	if err := s.umountPodVolumes(); err != nil {
		return errors.Wrapf(err, "umount pod volumes")
	}
	if timeout == 0 {
		timeout = 15
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	/*if err := s.getCRI().StopPod(ctx, &runtimeapi.StopPodSandboxRequest{
		PodSandboxId: s.getCRIId(),
	}); err != nil {
		return errors.Wrapf(err, "stop cri pod: %s", s.getCRIId())
	}*/
	if err := s.getCRI().RemovePod(ctx, s.getCRIId()); err != nil {
		return errors.Wrapf(err, "remove cri pod: %s", s.getCRIId())
	}
	return nil
}

func (s *sPodGuestInstance) LoadDesc() error {
	if err := LoadDesc(s); err != nil {
		return errors.Wrap(err, "LoadDesc")
	}
	if err := s.loadContainers(); err != nil {
		return errors.Wrap(err, "loadContainers")
	}
	return nil
}

func (s *sPodGuestInstance) loadContainers() error {
	s.containers = make(map[string]*sContainer)
	ctrFile := s.getContainersFilePath()
	if !fileutils2.Exists(ctrFile) {
		log.Warningf("pod %s containers file %s doesn't exist", s.Id, ctrFile)
		return nil
	}
	ctrStr, err := ioutil.ReadFile(ctrFile)
	if err != nil {
		return errors.Wrapf(err, "read %s", ctrFile)
	}
	obj, err := jsonutils.Parse(ctrStr)
	if err != nil {
		return errors.Wrapf(err, "jsonutils.Parse %s", ctrStr)
	}
	ctrs := make(map[string]*sContainer)
	if err := obj.Unmarshal(ctrs); err != nil {
		return errors.Wrapf(err, "unmarshal %s to container map", obj.String())
	}
	s.containers = ctrs
	return nil
}

func (s *sPodGuestInstance) PostLoad(m *SGuestManager) error {
	return nil
}

func (s *sPodGuestInstance) getContainerCRIId(ctrId string) (string, error) {
	ctr := s.getContainer(ctrId)
	if ctr == nil {
		return "", errors.Wrapf(errors.ErrNotFound, "Not found container %s", ctrId)
	}
	return ctr.CRIId, nil
}

func (s *sPodGuestInstance) StartContainer(ctx context.Context, userCred mcclient.TokenCredential, ctrId string, input *hostapi.ContainerCreateInput) (jsonutils.JSONObject, error) {
	_, hasCtr := s.containers[ctrId]
	needRecreate := false
	if hasCtr {
		status, err := s.getContainerStatus(ctx, ctrId)
		if err != nil {
			if strings.Contains(err.Error(), "not found") {
				needRecreate = true
			} else {
				return nil, errors.Wrap(err, "get container status")
			}
		} else {
			if status == computeapi.CONTAINER_STATUS_EXITED {
				needRecreate = true
			} else if status != computeapi.CONTAINER_STATUS_CREATED {
				return nil, errors.Wrapf(err, "can't start container when status is %s", status)
			}
		}
	}
	if !hasCtr || needRecreate {
		log.Infof("recreate container %s before starting. hasCtr: %v, needRecreate: %v", ctrId, hasCtr, needRecreate)
		// delete and recreate the container before starting
		if hasCtr {
			if _, err := s.DeleteContainer(ctx, userCred, ctrId); err != nil {
				return nil, errors.Wrap(err, "delete container before starting")
			}
		}
		if _, err := s.CreateContainer(ctx, userCred, ctrId, input); err != nil {
			return nil, errors.Wrap(err, "recreate container before starting")
		}
	}

	criId, err := s.getContainerCRIId(ctrId)
	if err != nil {
		return nil, errors.Wrap(err, "get container cri id")
	}
	if err := s.getCRI().StartContainer(ctx, criId); err != nil {
		return nil, errors.Wrap(err, "CRI.StartContainer")
	}
	if err := s.setContainerCgroupDevicesAllow(criId, input.Spec.CgroupDevicesAllow); err != nil {
		return nil, errors.Wrap(err, "set cgroup devices allow")
	}
	if err := s.doContainerStartPostLifecycle(ctx, criId, input); err != nil {
		return nil, errors.Wrap(err, "do container lifecycle")
	}
	return nil, nil
}

func (s *sPodGuestInstance) doContainerStartPostLifecycle(ctx context.Context, criId string, input *hostapi.ContainerCreateInput) error {
	ls := input.Spec.Lifecyle
	if ls == nil {
		return nil
	}
	if ls.PostStart == nil {
		return nil
	}
	drv := lifecycle.GetDriver(ls.PostStart.Type)
	if err := drv.Run(ctx, ls.PostStart, s.getCRI(), criId); err != nil {
		return errors.Wrapf(err, "run %s", ls.PostStart.Type)
	}
	return nil
}

func (s *sPodGuestInstance) StopContainer(ctx context.Context, userCred mcclient.TokenCredential, ctrId string, body jsonutils.JSONObject) (jsonutils.JSONObject, error) {
	criId, err := s.getContainerCRIId(ctrId)
	if err != nil {
		return nil, errors.Wrap(err, "get container cri id")
	}
	var timeout int64 = 0
	if body.Contains("timeout") {
		timeout, _ = body.Int("timeout")
	}
	if err := s.getCRI().StopContainer(context.Background(), criId, timeout); err != nil {
		return nil, errors.Wrap(err, "CRI.StopContainer")
	}
	return nil, nil
}

func (s *sPodGuestInstance) getCRIId() string {
	return s.GetSourceDesc().Metadata[computeapi.POD_METADATA_CRI_ID]
}

func (s *sPodGuestInstance) convertToPodMetadataPortMappings(cfg *runtimeapi.PodSandboxConfig) []*computeapi.PodMetadataPortMapping {
	if cfg.PortMappings == nil {
		return []*computeapi.PodMetadataPortMapping{}
	}
	ret := make([]*computeapi.PodMetadataPortMapping, len(cfg.PortMappings))
	for idx := range cfg.PortMappings {
		pm := cfg.PortMappings[idx]
		var proto computeapi.PodPortMappingProtocol = computeapi.PodPortMappingProtocolTCP
		if pm.Protocol == runtimeapi.Protocol_UDP {
			proto = computeapi.PodPortMappingProtocolUDP
		}
		ret[idx] = &computeapi.PodMetadataPortMapping{
			Protocol:      proto,
			ContainerPort: pm.ContainerPort,
			HostPort:      pm.HostPort,
			HostIp:        pm.HostIp,
		}
	}
	return ret
}

func (s *sPodGuestInstance) setCRIInfo(ctx context.Context, userCred mcclient.TokenCredential, criId string, cfg *runtimeapi.PodSandboxConfig) error {
	s.Desc.Metadata[computeapi.POD_METADATA_CRI_ID] = criId
	cfgStr := jsonutils.Marshal(cfg).String()
	s.Desc.Metadata[computeapi.POD_METADATA_CRI_CONFIG] = cfgStr

	pms := s.convertToPodMetadataPortMappings(cfg)
	pmStr := jsonutils.Marshal(pms).String()
	s.Desc.Metadata[computeapi.POD_METADATA_PORT_MAPPINGS] = pmStr

	session := auth.GetSession(ctx, userCred, options.HostOptions.Region)
	if _, err := computemod.Servers.SetMetadata(session, s.GetId(), jsonutils.Marshal(map[string]string{
		computeapi.POD_METADATA_CRI_ID:        criId,
		computeapi.POD_METADATA_CRI_CONFIG:    cfgStr,
		computeapi.POD_METADATA_PORT_MAPPINGS: pmStr,
	})); err != nil {
		return errors.Wrapf(err, "set cri_id of pod %s", s.GetId())
	}
	return SaveDesc(s, s.Desc)
}

func (s *sPodGuestInstance) setContainerCRIInfo(ctx context.Context, userCred mcclient.TokenCredential, ctrId, criId string) error {
	session := auth.GetSession(ctx, userCred, options.HostOptions.Region)
	if _, err := computemod.Containers.SetMetadata(session, ctrId, jsonutils.Marshal(map[string]string{
		computeapi.CONTAINER_METADATA_CRI_ID: criId,
	})); err != nil {
		return errors.Wrapf(err, "set cri_id of container %s", ctrId)
	}
	return nil
}

func (s *sPodGuestInstance) getPodSandboxConfig() (*runtimeapi.PodSandboxConfig, error) {
	cfgStr := s.GetSourceDesc().Metadata[computeapi.POD_METADATA_CRI_CONFIG]
	obj, err := jsonutils.ParseString(cfgStr)
	if err != nil {
		return nil, errors.Wrapf(err, "ParseString to json object: %s", cfgStr)
	}
	podCfg := new(runtimeapi.PodSandboxConfig)
	if err := obj.Unmarshal(podCfg); err != nil {
		return nil, errors.Wrap(err, "Unmarshal to PodSandboxConfig")
	}
	return podCfg, nil
}

func (s *sPodGuestInstance) GetPodMetadataPortMappings() ([]*computeapi.PodMetadataPortMapping, error) {
	cfgStr := s.GetSourceDesc().Metadata[computeapi.POD_METADATA_PORT_MAPPINGS]
	if cfgStr == "" {
		return nil, nil
	}
	obj, err := jsonutils.ParseString(cfgStr)
	if err != nil {
		return nil, errors.Wrapf(err, "ParseString to json object: %s", cfgStr)
	}
	pms := make([]*computeapi.PodMetadataPortMapping, 0)
	if err := obj.Unmarshal(pms); err != nil {
		return nil, errors.Wrap(err, "Unmarshal to PodMetadataPortMappings")
	}
	return pms, nil
}

func (s *sPodGuestInstance) saveContainer(id string, criId string) error {
	_, ok := s.containers[id]
	if ok {
		return errors.Errorf("container %s already exists", criId)
	}
	ctr := newContainer(id)
	ctr.CRIId = criId
	s.containers[id] = ctr
	if err := s.saveContainersFile(s.containers); err != nil {
		return errors.Wrap(err, "saveContainersFile")
	}
	return nil
}

func (s *sPodGuestInstance) saveContainersFile(containers map[string]*sContainer) error {
	content := jsonutils.Marshal(containers).String()
	if err := fileutils2.FilePutContents(s.getContainersFilePath(), content, false); err != nil {
		return errors.Wrapf(err, "put content %s to containers file", content)
	}
	return nil
}

func (s *sPodGuestInstance) getContainersFilePath() string {
	return path.Join(s.HomeDir(), "containers")
}

func (s *sPodGuestInstance) getContainer(id string) *sContainer {
	return s.containers[id]
}

func (s *sPodGuestInstance) CreateContainer(ctx context.Context, userCred mcclient.TokenCredential, id string, input *hostapi.ContainerCreateInput) (jsonutils.JSONObject, error) {
	ctrCriId, err := s.createContainer(ctx, userCred, id, input)
	if err != nil {
		return nil, errors.Wrap(err, "CRI.CreateContainer")
	}
	if err := s.setContainerCRIInfo(ctx, userCred, id, ctrCriId); err != nil {
		return nil, errors.Wrap(err, "setContainerCRIInfo")
	}
	return nil, nil
}

func (s *sPodGuestInstance) getContainerLogPath(ctrId string) string {
	return filepath.Join(fmt.Sprintf("%s.log", ctrId))
}

func (s *sPodGuestInstance) getLxcfsMounts() []*runtimeapi.Mount {
	// lxcfsPath := "/var/lib/lxc/lxcfs"
	lxcfsPath := options.HostOptions.LxcfsPath
	return []*runtimeapi.Mount{
		{
			ContainerPath: "/proc/uptime",
			HostPath:      fmt.Sprintf("%s/proc/uptime", lxcfsPath),
			Readonly:      true,
		},
		{
			ContainerPath: "/proc/meminfo",
			HostPath:      fmt.Sprintf("%s/proc/meminfo", lxcfsPath),
			Readonly:      true,
		},
		{
			ContainerPath: "/proc/stat",
			HostPath:      fmt.Sprintf("%s/proc/stat", lxcfsPath),
			Readonly:      true,
		},
		{
			ContainerPath: "/proc/cpuinfo",
			HostPath:      fmt.Sprintf("%s/proc/cpuinfo", lxcfsPath),
			Readonly:      true,
		},
		{
			ContainerPath: "/proc/swaps",
			HostPath:      fmt.Sprintf("%s/proc/swaps", lxcfsPath),
			Readonly:      true,
		},
		{
			ContainerPath: "/proc/diskstats",
			HostPath:      fmt.Sprintf("%s/proc/diskstats", lxcfsPath),
			Readonly:      true,
		},
	}
}

func (s *sPodGuestInstance) getContainerMounts(input *hostapi.ContainerCreateInput) ([]*runtimeapi.Mount, error) {
	inputMounts := input.Spec.VolumeMounts
	if len(inputMounts) == 0 {
		return make([]*runtimeapi.Mount, 0), nil
	}
	mounts := make([]*runtimeapi.Mount, len(inputMounts))

	for idx, im := range inputMounts {
		mnt := &runtimeapi.Mount{
			ContainerPath:  im.MountPath,
			Readonly:       im.ReadOnly,
			SelinuxRelabel: im.SelinuxRelabel,
			Propagation:    volume_mount.GetRuntimeVolumeMountPropagation(im.Propagation),
		}
		hostPath, err := volume_mount.GetDriver(im.Type).GetRuntimeMountHostPath(s, im)
		if err != nil {
			return nil, errors.Wrapf(err, "get runtime host mount path of %s", jsonutils.Marshal(im))
		}
		mnt.HostPath = hostPath
		mounts[idx] = mnt
	}
	return mounts, nil
}

func (s *sPodGuestInstance) getContainerCgroupDir(dirType string, ctrId string) string {
	cgroupDir := "/sys/fs/cgroup"
	return filepath.Join(cgroupDir, dirType, s.getCgroupParent(), ctrId)
}

func (s *sPodGuestInstance) getContainerCgroupDevicesDir(ctrId string) string {
	return s.getContainerCgroupDir("devices", ctrId)
}

func (s *sPodGuestInstance) setContainerCgroupDevicesAllow(ctrId string, allowStrs []string) error {
	for _, allowStr := range allowStrs {
		deviceAllowFile := filepath.Join(s.getContainerCgroupDevicesDir(ctrId), "devices.allow")
		out, err := procutils.NewRemoteCommandAsFarAsPossible("sh", "-c", fmt.Sprintf("echo '%s' > %s", allowStr, deviceAllowFile)).Output()
		if err != nil {
			return errors.Wrapf(err, "echo %s to %s: %s", deviceAllowFile, allowStr, out)
		}
	}
	return nil
}

func (s *sPodGuestInstance) createContainer(ctx context.Context, userCred mcclient.TokenCredential, ctrId string, input *hostapi.ContainerCreateInput) (string, error) {
	log.Infof("=====container input: %s", jsonutils.Marshal(input).PrettyString())
	podCfg, err := s.getPodSandboxConfig()
	if err != nil {
		return "", errors.Wrap(err, "getPodSandboxConfig")
	}
	mounts, err := s.getContainerMounts(input)
	if err != nil {
		return "", errors.Wrap(err, "get container mounts")
	}

	// REF: https://docs.docker.com/config/containers/resource_constraints/#configure-the-default-cfs-scheduler
	var defaultCPUPeriod int64 = 1000
	spec := input.Spec
	ctrCfg := &runtimeapi.ContainerConfig{
		Metadata: &runtimeapi.ContainerMetadata{
			Name: input.Name,
		},
		Image: &runtimeapi.ImageSpec{
			Image: spec.Image,
		},
		Linux: &runtimeapi.LinuxContainerConfig{
			Resources: &runtimeapi.LinuxContainerResources{
				CpuPeriod: defaultCPUPeriod,
				//CpuQuota:               s.GetDesc().Cpu * defaultCPUPeriod,
				//CpuShares:              defaultCPUPeriod,
				MemoryLimitInBytes:     s.GetDesc().Mem * 1024 * 1024,
				OomScoreAdj:            0,
				CpusetCpus:             "",
				CpusetMems:             "",
				HugepageLimits:         nil,
				Unified:                nil,
				MemorySwapLimitInBytes: 0,
			},
			SecurityContext: &runtimeapi.LinuxContainerSecurityContext{
				Capabilities:       &runtimeapi.Capability{},
				Privileged:         spec.Privileged,
				NamespaceOptions:   nil,
				SelinuxOptions:     nil,
				RunAsUser:          nil,
				RunAsGroup:         nil,
				RunAsUsername:      "",
				ReadonlyRootfs:     false,
				SupplementalGroups: nil,
				NoNewPrivs:         true,
				MaskedPaths:        nil,
				ReadonlyPaths:      nil,
				Seccomp: &runtimeapi.SecurityProfile{
					ProfileType: runtimeapi.SecurityProfile_Unconfined,
				},
				Apparmor: &runtimeapi.SecurityProfile{
					ProfileType: runtimeapi.SecurityProfile_Unconfined,
				},
				ApparmorProfile:    "",
				SeccompProfilePath: "",
			},
		},
		LogPath: s.getContainerLogPath(ctrId),
		Envs:    make([]*runtimeapi.KeyValue, 0),
		Devices: []*runtimeapi.Device{},
		Mounts:  mounts,
	}
	if spec.EnableLxcfs {
		ctrCfg.Mounts = append(ctrCfg.Mounts, s.getLxcfsMounts()...)
	}
	if spec.Capabilities != nil {
		ctrCfg.Linux.SecurityContext.Capabilities.AddCapabilities = spec.Capabilities.Add
		ctrCfg.Linux.SecurityContext.Capabilities.DropCapabilities = spec.Capabilities.Drop
	}
	for _, env := range spec.Envs {
		ctrCfg.Envs = append(ctrCfg.Envs, &runtimeapi.KeyValue{
			Key:   env.Key,
			Value: env.Value,
		})
	}
	if len(spec.Devices) != 0 {
		for _, dev := range spec.Devices {
			ctrDevs, err := device.GetDriver(dev.Type).GetRuntimeDevices(input, dev)
			if err != nil {
				return "", errors.Wrapf(err, "GetRuntimeDevices of %s", jsonutils.Marshal(dev))
			}
			ctrCfg.Devices = append(ctrCfg.Devices, ctrDevs...)
		}

		nvMan, err := isolated_device.GetContainerDeviceManager(isolated_device.ContainerDeviceTypeNVIDIAGPU)
		if err != nil {
			return "", errors.Wrapf(err, "GetContainerDeviceManager by type %q", isolated_device.ContainerDeviceTypeNVIDIAGPU)
		}
		if envs := nvMan.GetContainerEnvs(spec.Devices); len(envs) > 0 {
			ctrCfg.Envs = append(ctrCfg.Envs, envs...)
		}
	}
	if len(spec.Command) != 0 {
		ctrCfg.Command = spec.Command
	}
	if len(spec.Args) != 0 {
		ctrCfg.Args = spec.Args
	}
	criId, err := s.getCRI().CreateContainer(ctx, s.getCRIId(), podCfg, ctrCfg, false)
	if err != nil {
		return "", errors.Wrap(err, "cri.CreateContainer")
	}
	if err := s.saveContainer(ctrId, criId); err != nil {
		return "", errors.Wrap(err, "saveContainer")
	}
	return criId, nil
}

func (s *sPodGuestInstance) DeleteContainer(ctx context.Context, userCred mcclient.TokenCredential, ctrId string) (jsonutils.JSONObject, error) {
	criId, err := s.getContainerCRIId(ctrId)
	if err != nil && errors.Cause(err) != errors.ErrNotFound {
		return nil, errors.Wrap(err, "getContainerCRIId")
	}
	if criId != "" {
		if err := s.getCRI().RemoveContainer(ctx, criId); err != nil && !strings.Contains(err.Error(), "not found") {
			return nil, errors.Wrap(err, "cri.RemoveContainer")
		}
	}
	// refresh local containers file
	delete(s.containers, ctrId)
	if err := s.saveContainersFile(s.containers); err != nil {
		return nil, errors.Wrap(err, "saveContainersFile")
	}
	return nil, nil
}

func (s *sPodGuestInstance) getContainerStatus(ctx context.Context, ctrId string) (string, error) {
	criId, err := s.getContainerCRIId(ctrId)
	if err != nil {
		return "", errors.Wrapf(err, "get container cri_id by %s", ctrId)
	}
	resp, err := s.getCRI().ContainerStatus(ctx, criId)
	if err != nil {
		return "", errors.Wrap(err, "cri.ContainerStatus")
	}
	status := computeapi.CONTAINER_STATUS_UNKNOWN
	switch resp.Status.State {
	case runtimeapi.ContainerState_CONTAINER_CREATED:
		status = computeapi.CONTAINER_STATUS_CREATED
	case runtimeapi.ContainerState_CONTAINER_RUNNING:
		status = computeapi.CONTAINER_STATUS_RUNNING
	case runtimeapi.ContainerState_CONTAINER_EXITED:
		status = computeapi.CONTAINER_STATUS_EXITED
	case runtimeapi.ContainerState_CONTAINER_UNKNOWN:
		status = computeapi.CONTAINER_STATUS_UNKNOWN
	}
	return status, nil
}

func (s *sPodGuestInstance) SyncContainerStatus(ctx context.Context, userCred mcclient.TokenCredential, ctrId string) (jsonutils.JSONObject, error) {
	status, err := s.getContainerStatus(ctx, ctrId)
	if err != nil {
		return nil, errors.Wrap(err, "get container status")
	}
	return jsonutils.Marshal(computeapi.ContainerSyncStatusResponse{Status: status}), nil
}

func (s *sPodGuestInstance) PullImage(ctx context.Context, userCred mcclient.TokenCredential, ctrId string, input *hostapi.ContainerPullImageInput) (jsonutils.JSONObject, error) {
	policy := input.PullPolicy
	if policy == apis.ImagePullPolicyIfNotPresent || policy == "" {
		// check if image is presented
		img, err := s.getCRI().ImageStatus(ctx, &runtimeapi.ImageStatusRequest{
			Image: &runtimeapi.ImageSpec{
				Image: input.Image,
			},
		})
		if err != nil {
			return nil, errors.Wrapf(err, "cri.ImageStatus %s", input.Image)
		}
		if img.Image != nil {
			log.Infof("image %s already exists, skipping pulling it when policy is %s", input.Image, policy)
			return jsonutils.Marshal(&runtimeapi.PullImageResponse{
				ImageRef: img.Image.Id,
			}), nil
		}
	}
	podCfg, err := s.getPodSandboxConfig()
	if err != nil {
		return nil, errors.Wrap(err, "get pod sandbox config")
	}
	req := &runtimeapi.PullImageRequest{
		Image: &runtimeapi.ImageSpec{
			Image: input.Image,
		},
		SandboxConfig: podCfg,
	}
	if input.Auth != nil {
		authCfg := &runtimeapi.AuthConfig{
			Username:      input.Auth.Username,
			Password:      input.Auth.Password,
			Auth:          input.Auth.Auth,
			ServerAddress: input.Auth.ServerAddress,
			IdentityToken: input.Auth.IdentityToken,
			RegistryToken: input.Auth.RegistryToken,
		}
		req.Auth = authCfg
	}
	resp, err := s.getCRI().PullImage(ctx, req)
	if err != nil {
		return nil, errors.Wrapf(err, "cri.PullImage %s", input.Image)
	}
	return jsonutils.Marshal(resp), nil
}
