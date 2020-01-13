/*
Copyright 2019 Cortex Labs, Inc.

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

package operator

import (
	"time"

	kapps "k8s.io/api/apps/v1"
	kcore "k8s.io/api/core/v1"
	kmeta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cortexlabs/cortex/pkg/lib/errors"
	"github.com/cortexlabs/cortex/pkg/lib/k8s"
	"github.com/cortexlabs/cortex/pkg/lib/parallel"
	"github.com/cortexlabs/cortex/pkg/lib/telemetry"
	"github.com/cortexlabs/cortex/pkg/operator/config"
)

func operatorCron() error {
	deployments, failedPods, err := getCronK8sResources()
	if err != nil {
		return err
	}

	return parallel.RunFirstErr(
		deleteEvictedPods(failedPods),
		updateHPAs(deployments),
	)
}

func deleteEvictedPods(failedPods []kcore.Pod) error {
	var errs []error

	evictedPods := []kcore.Pod{}
	for _, pod := range failedPods {
		if pod.Status.Reason == k8s.ReasonEvicted {
			_, err := config.Kubernetes.DeletePod(pod.Name)
			if err != nil {
				errs = append(errs, err)
			}
		}
	}

	if errors.HasError(errs) {
		return errors.FirstError(errs)
	}
	return nil
}

func updateHPAs(deployments []kapps.Deployment) error {
	var allPods []kcore.Pod = nil

	var errs []error

	for _, deployment := range deployments {
		if config.Kubernetes.HPAExists(deployment.Labels["apiName"]) {
			continue // since the HPA is deleted every time the deployment is updated
		}

		if allPods == nil {
			var err error
			allPods, err = config.Kubernetes.ListPodsWithLabels("apiName")
			if err != nil {
				errs = append(errs, err)
				continue
			}
		}

		updatedReplicas := numUpdatedReadyReplicas(&deployment, allPods)
		if updatedReplicas < deployment.Spec.Replicas {
			continue // not yet up-to-date
		}

		for _, condition := range deployment.Status.Conditions {
			if condition.Type == kapps.DeploymentProgressing &&
				condition.Status == kcore.ConditionTrue &&
				!condition.LastUpdateTime.IsZero() &&
				time.Now().After(condition.LastUpdateTime.Add(35*time.Second)) { // the metrics poll interval is 30 seconds, so 35 should be safe

				_, err := config.Kubernetes.CreateHPA(hpaSpec(deployment))
				if err != nil {
					errs = append(errs, err)
					continue
				}
			}
		}
	}

	if errors.HasError(errs) {
		return errors.FirstError(errs)
	}
	return nil
}

func numUpdatedReadyReplicas(deployment *kapps.Deployment, pods []kcore.Pod) int32 {
	var readyReplicas int32
	for _, pod := range pods {
		if pod.Labels["apiName"] != deployment.Labels["apiName"] {
			continue
		}
		if k8s.IsPodReady(&pod) && k8s.IsPodSpecLatest(&pod, deployment) {
			readyReplicas++
		}
	}

	return readyReplicas
}

func getCronK8sResources() ([]kapps.Deployment, []kcore.Pod, error) {
	var deployments []kapps.Deployment
	var failedPods []kcore.Pod

	err := parallel.RunFirstErr(
		func() error {
			var err error
			deployments, err = config.Kubernetes.ListDeploymentsWithLabels("apiName")
			return err
		},
		func() error {
			var err error
			failedPods, err = config.Kubernetes.ListPods(&kmeta.ListOptions{
				FieldSelector: "status.phase=Failed",
			})
			return err
		},
	)

	return deployments, failedPods, err
}

func telemetryCron() error {
	nodes, err := config.Kubernetes.ListNodes(nil)
	if err != nil {
		return err
	}

	instanceTypeCounts := make(map[string]int)
	var totalInstances int

	for _, node := range nodes {
		if node.Labels["workload"] != "true" {
			continue
		}

		instanceType := node.Labels["beta.kubernetes.io/instance-type"]
		if instanceType == "" {
			instanceType = "unknown"
		}

		instanceTypeCounts[instanceType]++
		totalInstances++
	}

	properties := map[string]interface{}{
		"instanceTypes": instanceTypeCounts,
		"instanceCount": totalInstances,
	}

	telemetry.Event("operator.cron", properties)
}

func cronErrHandler(cronName string) func(error) {
	return func(error) {
		err = errors.Wrap(cronName " cron failed")
		telemetry.Error(err)
		errors.PrintError(err)
	}
}
