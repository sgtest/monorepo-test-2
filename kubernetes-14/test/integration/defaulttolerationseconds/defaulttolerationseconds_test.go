// +build integration,!no-etcd

/*
Copyright 2017 The Kubernetes Authors.

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

package defaulttolerationseconds

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	restclient "k8s.io/client-go/rest"
	"github.com/sourcegraph/monorepo-test-1/kubernetes-14/pkg/api"
	"github.com/sourcegraph/monorepo-test-1/kubernetes-14/pkg/api/helper"
	"github.com/sourcegraph/monorepo-test-1/kubernetes-14/pkg/api/v1"
	"github.com/sourcegraph/monorepo-test-1/kubernetes-14/pkg/client/clientset_generated/clientset"
	"github.com/sourcegraph/monorepo-test-1/kubernetes-14/plugin/pkg/admission/defaulttolerationseconds"
	"github.com/sourcegraph/monorepo-test-1/kubernetes-14/test/integration/framework"
)

func TestAdmission(t *testing.T) {
	masterConfig := framework.NewMasterConfig()
	masterConfig.GenericConfig.EnableProfiling = true
	masterConfig.GenericConfig.EnableMetrics = true
	masterConfig.GenericConfig.AdmissionControl = defaulttolerationseconds.NewDefaultTolerationSeconds()
	_, s := framework.RunAMaster(masterConfig)
	defer s.Close()

	client := clientset.NewForConfigOrDie(&restclient.Config{Host: s.URL, ContentConfig: restclient.ContentConfig{GroupVersion: &api.Registry.GroupOrDie(v1.GroupName).GroupVersion}})

	ns := framework.CreateTestingNamespace("default-toleration-seconds", s, t)
	defer framework.DeleteTestingNamespace(ns, s, t)

	pod := v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns.Name,
			Name:      "foo",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "test",
					Image: "an-image",
				},
			},
		},
	}

	updatedPod, err := client.Core().Pods(pod.Namespace).Create(&pod)
	if err != nil {
		t.Fatalf("error creating pod: %v", err)
	}

	var defaultSeconds int64 = 300
	nodeNotReady := v1.Toleration{
		Key:               metav1.TaintNodeNotReady,
		Operator:          v1.TolerationOpExists,
		Effect:            v1.TaintEffectNoExecute,
		TolerationSeconds: &defaultSeconds,
	}

	nodeUnreachable := v1.Toleration{
		Key:               metav1.TaintNodeUnreachable,
		Operator:          v1.TolerationOpExists,
		Effect:            v1.TaintEffectNoExecute,
		TolerationSeconds: &defaultSeconds,
	}

	found := 0
	tolerations := updatedPod.Spec.Tolerations
	for i := range tolerations {
		if found == 2 {
			break
		}
		if tolerations[i].MatchToleration(&nodeNotReady) {
			if helper.Semantic.DeepEqual(tolerations[i], nodeNotReady) {
				found++
				continue
			}
		}
		if tolerations[i].MatchToleration(&nodeUnreachable) {
			if helper.Semantic.DeepEqual(tolerations[i], nodeUnreachable) {
				found++
				continue
			}
		}
	}

	if found != 2 {
		t.Fatalf("unexpected tolerations: %v\n", updatedPod.Spec.Tolerations)
	}
}
