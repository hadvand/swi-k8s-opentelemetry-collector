// Copyright 2022 SolarWinds Worldwide, LLC. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Source: https://github.com/open-telemetry/opentelemetry-collector-contrib
// Changes customizing the original source code: see CHANGELOG.md in deploy/helm directory

package swk8sattributesprocessor // import "github.com/solarwinds/swi-k8s-opentelemetry-collector/processor/swk8sattributesprocessor"

import (
	"context"
	"fmt"
	"strconv"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componentstatus"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	conventions "go.opentelemetry.io/collector/semconv/v1.8.0"
	"go.uber.org/zap"

	"github.com/solarwinds/swi-k8s-opentelemetry-collector/internal/k8sconfig"
	"github.com/solarwinds/swi-k8s-opentelemetry-collector/processor/swk8sattributesprocessor/internal/kube"
)

const (
	clientIPLabelName string = "ip"
)

type kubernetesprocessor struct {
	cfg                component.Config
	options            []option
	telemetrySettings  component.TelemetrySettings
	logger             *zap.Logger
	apiConfig          k8sconfig.APIConfig
	kc                 kube.Client
	passthroughMode    bool
	setObjectExistence bool
	rules              kube.ExtractionRules
	filters            kube.Filters
	podAssociations    []kube.Association
	podIgnore          kube.Excludes

	resources map[string]*kubernetesProcessorResource
}

func (kp *kubernetesprocessor) initKubeClient(set component.TelemetrySettings, kubeClient kube.ClientProvider) error {
	if kubeClient == nil {
		kubeClient = kube.New
	}
	if !kp.passthroughMode {
		kc, err := kubeClient(
			set,
			kp.apiConfig,
			kp.rules,
			kp.filters,
			kp.podAssociations,
			kp.podIgnore,
			nil,
			nil,
			nil,
			map[string]*kube.ClientResource{
				kube.MetadataFromDeployment:            kp.getClientResource(kp.resources[kube.MetadataFromDeployment]),
				kube.MetadataFromStatefulSet:           kp.getClientResource(kp.resources[kube.MetadataFromStatefulSet]),
				kube.MetadataFromReplicaSet:            kp.getClientResource(kp.resources[kube.MetadataFromReplicaSet]),
				kube.MetadataFromDaemonSet:             kp.getClientResource(kp.resources[kube.MetadataFromDaemonSet]),
				kube.MetadataFromJob:                   kp.getClientResource(kp.resources[kube.MetadataFromJob]),
				kube.MetadataFromCronJob:               kp.getClientResource(kp.resources[kube.MetadataFromCronJob]),
				kube.MetadataFromNode:                  kp.getClientResource(kp.resources[kube.MetadataFromNode]),
				kube.MetadataFromPersistentVolume:      kp.getClientResource(kp.resources[kube.MetadataFromPersistentVolume]),
				kube.MetadataFromPersistentVolumeClaim: kp.getClientResource(kp.resources[kube.MetadataFromPersistentVolumeClaim]),
				kube.MetadataFromService:               kp.getClientResource(kp.resources[kube.MetadataFromService]),
			})
		if err != nil {
			return err
		}
		kp.kc = kc
	}
	return nil
}

func (kp *kubernetesprocessor) Start(_ context.Context, host component.Host) error {
	allOptions := append(createProcessorOpts(kp.cfg), kp.options...)

	for _, opt := range allOptions {
		if err := opt(kp); err != nil {
			componentstatus.ReportStatus(host, componentstatus.NewFatalErrorEvent(err))
			return nil
		}

	}

	// This might have been set by an option already
	if kp.kc == nil {
		err := kp.initKubeClient(kp.telemetrySettings, kubeClientProvider)
		if err != nil {
			componentstatus.ReportStatus(host, componentstatus.NewFatalErrorEvent(err))
			return nil
		}
	}
	if !kp.passthroughMode {
		go kp.kc.Start()
	}
	return nil
}

func (kp *kubernetesprocessor) Shutdown(context.Context) error {
	if kp.kc == nil {
		return nil
	}
	if !kp.passthroughMode {
		kp.kc.Stop()
	}
	return nil
}

// processTraces process traces and add k8s metadata using resource IP or incoming IP as pod origin.
func (kp *kubernetesprocessor) processTraces(ctx context.Context, td ptrace.Traces) (ptrace.Traces, error) {
	rss := td.ResourceSpans()
	for i := 0; i < rss.Len(); i++ {
		kp.processResource(ctx, rss.At(i).Resource())
	}

	return td, nil
}

// processMetrics process metrics and add k8s metadata using resource IP, hostname or incoming IP as pod origin.
func (kp *kubernetesprocessor) processMetrics(ctx context.Context, md pmetric.Metrics) (pmetric.Metrics, error) {
	rm := md.ResourceMetrics()
	for i := 0; i < rm.Len(); i++ {
		kp.processResource(ctx, rm.At(i).Resource())
	}

	return md, nil
}

// processLogs process logs and add k8s metadata using resource IP, hostname or incoming IP as pod origin.
func (kp *kubernetesprocessor) processLogs(ctx context.Context, ld plog.Logs) (plog.Logs, error) {
	rl := ld.ResourceLogs()
	for i := 0; i < rl.Len(); i++ {
		kp.processResource(ctx, rl.At(i).Resource())
	}

	return ld, nil
}

// processResource adds Pod metadata tags to resource based on pod association configuration
func (kp *kubernetesprocessor) processResource(ctx context.Context, resource pcommon.Resource) {
	podIdentifierValue := extractPodID(ctx, resource.Attributes(), kp.podAssociations)
	kp.logger.Debug("evaluating pod identifier", zap.Any("value", podIdentifierValue))

	for i := range podIdentifierValue {
		if podIdentifierValue[i].Source.From == kube.ConnectionSource && podIdentifierValue[i].Value != "" {
			if _, found := resource.Attributes().Get(kube.K8sIPLabelName); !found {
				resource.Attributes().PutStr(kube.K8sIPLabelName, podIdentifierValue[i].Value)
			}
			break
		}
	}
	if kp.passthroughMode {
		return
	}

	var pod *kube.Pod
	if podIdentifierValue.IsNotEmpty() {
		var podFound bool
		if pod, podFound = kp.kc.GetPod(podIdentifierValue); podFound {
			kp.logger.Debug("getting the pod", zap.Any("pod", pod))

			for key, val := range pod.Attributes {
				if _, found := resource.Attributes().Get(key); !found {
					resource.Attributes().PutStr(key, val)
				}
			}
			kp.addContainerAttributes(resource.Attributes(), pod)
		}
	}

	namespace := getNamespace(pod, resource.Attributes())
	if namespace != "" {
		attrsToAdd := kp.getAttributesForPodsNamespace(namespace)
		for key, val := range attrsToAdd {
			if _, found := resource.Attributes().Get(key); !found {
				resource.Attributes().PutStr(key, val)
			}
		}
	}

	for resourceType, k8sResource := range kp.resources {
		if k8sResource != nil && !k8sResource.isEmpty() {
			processGenericResource(kp, resourceType, k8sResource.associations, ctx, resource)
		}
	}
}

func getNamespace(pod *kube.Pod, resAttrs pcommon.Map) string {
	if pod != nil && pod.Namespace != "" {
		return pod.Namespace
	}
	return stringAttributeFromMap(resAttrs, conventions.AttributeK8SNamespaceName)
}

// addContainerAttributes looks if pod has any container identifiers and adds additional container attributes
func (kp *kubernetesprocessor) addContainerAttributes(attrs pcommon.Map, pod *kube.Pod) {
	containerName := stringAttributeFromMap(attrs, conventions.AttributeK8SContainerName)
	containerID := stringAttributeFromMap(attrs, conventions.AttributeContainerID)
	var (
		containerSpec *kube.Container
		ok            bool
	)
	switch {
	case containerName != "":
		containerSpec, ok = pod.Containers.ByName[containerName]
		if !ok {
			return
		}
	case containerID != "":
		containerSpec, ok = pod.Containers.ByID[containerID]
		if !ok {
			return
		}
	default:
		return
	}
	if containerSpec.Name != "" {
		if _, found := attrs.Get(conventions.AttributeK8SContainerName); !found {
			attrs.PutStr(conventions.AttributeK8SContainerName, containerSpec.Name)
		}
	}
	if containerSpec.ImageName != "" {
		if _, found := attrs.Get(conventions.AttributeContainerImageName); !found {
			attrs.PutStr(conventions.AttributeContainerImageName, containerSpec.ImageName)
		}
	}
	if containerSpec.ImageTag != "" {
		if _, found := attrs.Get(conventions.AttributeContainerImageTag); !found {
			attrs.PutStr(conventions.AttributeContainerImageTag, containerSpec.ImageTag)
		}
	}
	// attempt to get container ID from restart count
	runID := -1
	runIDAttr, ok := attrs.Get(conventions.AttributeK8SContainerRestartCount)
	if ok {
		containerRunID, err := intFromAttribute(runIDAttr)
		if err != nil {
			kp.logger.Debug(err.Error())
		} else {
			runID = containerRunID
		}
	} else {
		// take the highest runID (restart count) which represents the currently running container in most cases
		for containerRunID := range containerSpec.Statuses {
			if containerRunID > runID {
				runID = containerRunID
			}
		}
	}
	if runID != -1 {
		if containerStatus, ok := containerSpec.Statuses[runID]; ok {
			if _, found := attrs.Get(conventions.AttributeContainerID); !found && containerStatus.ContainerID != "" {
				attrs.PutStr(conventions.AttributeContainerID, containerStatus.ContainerID)
			}
			if _, found := attrs.Get(containerImageRepoDigests); !found && containerStatus.ImageRepoDigest != "" {
				attrs.PutEmptySlice(containerImageRepoDigests).AppendEmpty().SetStr(containerStatus.ImageRepoDigest)
			}

		}
	}
}

func (kp *kubernetesprocessor) getAttributesForPodsNamespace(namespace string) map[string]string {
	ns, ok := kp.kc.GetNamespace(namespace)
	if !ok {
		return nil
	}
	return ns.Attributes
}

// intFromAttribute extracts int value from an attribute stored as string or int
func intFromAttribute(val pcommon.Value) (int, error) {
	switch val.Type() {
	case pcommon.ValueTypeInt:
		return int(val.Int()), nil
	case pcommon.ValueTypeStr:
		i, err := strconv.Atoi(val.Str())
		if err != nil {
			return 0, err
		}
		return i, nil
	case pcommon.ValueTypeEmpty, pcommon.ValueTypeDouble, pcommon.ValueTypeBool, pcommon.ValueTypeMap, pcommon.ValueTypeSlice, pcommon.ValueTypeBytes:
		fallthrough
	default:
		return 0, fmt.Errorf("wrong attribute type %v, expected int", val.Type())
	}
}
