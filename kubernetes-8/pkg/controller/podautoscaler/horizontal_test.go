/*
Copyright 2015 The Kubernetes Authors.

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

package podautoscaler

import (
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	clientfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/pkg/api"
	clientv1 "k8s.io/client-go/pkg/api/v1"
	core "k8s.io/client-go/testing"
	"github.com/sourcegraph/monorepo-test-1/kubernetes-8/pkg/api/v1"
	autoscalingv1 "github.com/sourcegraph/monorepo-test-1/kubernetes-8/pkg/apis/autoscaling/v1"
	autoscalingv2 "github.com/sourcegraph/monorepo-test-1/kubernetes-8/pkg/apis/autoscaling/v2alpha1"
	extensions "github.com/sourcegraph/monorepo-test-1/kubernetes-8/pkg/apis/extensions/v1beta1"
	"github.com/sourcegraph/monorepo-test-1/kubernetes-8/pkg/client/clientset_generated/clientset/fake"
	informers "github.com/sourcegraph/monorepo-test-1/kubernetes-8/pkg/client/informers/informers_generated/externalversions"
	"github.com/sourcegraph/monorepo-test-1/kubernetes-8/pkg/controller"
	"github.com/sourcegraph/monorepo-test-1/kubernetes-8/pkg/controller/podautoscaler/metrics"
	metricsfake "k8s.io/metrics/pkg/client/clientset_generated/clientset/fake"
	cmfake "k8s.io/metrics/pkg/client/custom_metrics/fake"

	cmapi "k8s.io/metrics/pkg/apis/custom_metrics/v1alpha1"
	metricsapi "k8s.io/metrics/pkg/apis/metrics/v1alpha1"

	"github.com/stretchr/testify/assert"

	_ "github.com/sourcegraph/monorepo-test-1/kubernetes-8/pkg/apis/autoscaling/install"
	_ "github.com/sourcegraph/monorepo-test-1/kubernetes-8/pkg/apis/extensions/install"
)

func alwaysReady() bool { return true }

type fakeResource struct {
	name       string
	apiVersion string
	kind       string
}

type testCase struct {
	sync.Mutex
	minReplicas     int32
	maxReplicas     int32
	initialReplicas int32
	desiredReplicas int32

	// CPU target utilization as a percentage of the requested resources.
	CPUTarget            int32
	CPUCurrent           int32
	verifyCPUCurrent     bool
	reportedLevels       []uint64
	reportedCPURequests  []resource.Quantity
	reportedPodReadiness []v1.ConditionStatus
	scaleUpdated         bool
	statusUpdated        bool
	eventCreated         bool
	verifyEvents         bool
	useMetricsApi        bool
	metricsTarget        []autoscalingv2.MetricSpec
	// Channel with names of HPA objects which we have reconciled.
	processed chan string

	// Target resource information.
	resource *fakeResource

	// Last scale time
	lastScaleTime *metav1.Time
}

// Needs to be called under a lock.
func (tc *testCase) computeCPUCurrent() {
	if len(tc.reportedLevels) != len(tc.reportedCPURequests) || len(tc.reportedLevels) == 0 {
		return
	}
	reported := 0
	for _, r := range tc.reportedLevels {
		reported += int(r)
	}
	requested := 0
	for _, req := range tc.reportedCPURequests {
		requested += int(req.MilliValue())
	}
	tc.CPUCurrent = int32(100 * reported / requested)
}

func (tc *testCase) prepareTestClient(t *testing.T) (*fake.Clientset, *metricsfake.Clientset, *cmfake.FakeCustomMetricsClient) {
	namespace := "test-namespace"
	hpaName := "test-hpa"
	podNamePrefix := "test-pod"
	// TODO: also test with TargetSelector
	selector := map[string]string{"name": podNamePrefix}

	tc.Lock()

	tc.scaleUpdated = false
	tc.statusUpdated = false
	tc.eventCreated = false
	tc.processed = make(chan string, 100)
	if tc.CPUCurrent == 0 {
		tc.computeCPUCurrent()
	}

	// TODO(madhusudancs): HPA only supports resources in extensions/v1beta1 right now. Add
	// tests for "v1" replicationcontrollers when HPA adds support for cross-group scale.
	if tc.resource == nil {
		tc.resource = &fakeResource{
			name:       "test-rc",
			apiVersion: "extensions/v1beta1",
			kind:       "replicationcontrollers",
		}
	}
	tc.Unlock()

	fakeClient := &fake.Clientset{}
	fakeClient.AddReactor("list", "horizontalpodautoscalers", func(action core.Action) (handled bool, ret runtime.Object, err error) {
		tc.Lock()
		defer tc.Unlock()

		obj := &autoscalingv2.HorizontalPodAutoscalerList{
			Items: []autoscalingv2.HorizontalPodAutoscaler{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      hpaName,
						Namespace: namespace,
						SelfLink:  "experimental/v1/namespaces/" + namespace + "/horizontalpodautoscalers/" + hpaName,
					},
					Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
						ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
							Kind:       tc.resource.kind,
							Name:       tc.resource.name,
							APIVersion: tc.resource.apiVersion,
						},
						MinReplicas: &tc.minReplicas,
						MaxReplicas: tc.maxReplicas,
					},
					Status: autoscalingv2.HorizontalPodAutoscalerStatus{
						CurrentReplicas: tc.initialReplicas,
						DesiredReplicas: tc.initialReplicas,
					},
				},
			},
		}

		if tc.CPUTarget > 0.0 {
			obj.Items[0].Spec.Metrics = []autoscalingv2.MetricSpec{
				{
					Type: autoscalingv2.ResourceMetricSourceType,
					Resource: &autoscalingv2.ResourceMetricSource{
						Name: v1.ResourceCPU,
						TargetAverageUtilization: &tc.CPUTarget,
					},
				},
			}
		}
		if len(tc.metricsTarget) > 0 {
			obj.Items[0].Spec.Metrics = append(obj.Items[0].Spec.Metrics, tc.metricsTarget...)
		}

		if len(obj.Items[0].Spec.Metrics) == 0 {
			// manually add in the defaulting logic
			obj.Items[0].Spec.Metrics = []autoscalingv2.MetricSpec{
				{
					Type: autoscalingv2.ResourceMetricSourceType,
					Resource: &autoscalingv2.ResourceMetricSource{
						Name: v1.ResourceCPU,
					},
				},
			}
		}

		// and... convert to autoscaling v1 to return the right type
		objv1, err := UnsafeConvertToVersionVia(obj, autoscalingv1.SchemeGroupVersion)
		if err != nil {
			return true, nil, err
		}

		return true, objv1, nil
	})

	fakeClient.AddReactor("get", "replicationcontrollers", func(action core.Action) (handled bool, ret runtime.Object, err error) {
		tc.Lock()
		defer tc.Unlock()

		obj := &extensions.Scale{
			ObjectMeta: metav1.ObjectMeta{
				Name:      tc.resource.name,
				Namespace: namespace,
			},
			Spec: extensions.ScaleSpec{
				Replicas: tc.initialReplicas,
			},
			Status: extensions.ScaleStatus{
				Replicas: tc.initialReplicas,
				Selector: selector,
			},
		}
		return true, obj, nil
	})

	fakeClient.AddReactor("get", "deployments", func(action core.Action) (handled bool, ret runtime.Object, err error) {
		tc.Lock()
		defer tc.Unlock()

		obj := &extensions.Scale{
			ObjectMeta: metav1.ObjectMeta{
				Name:      tc.resource.name,
				Namespace: namespace,
			},
			Spec: extensions.ScaleSpec{
				Replicas: tc.initialReplicas,
			},
			Status: extensions.ScaleStatus{
				Replicas: tc.initialReplicas,
				Selector: selector,
			},
		}
		return true, obj, nil
	})

	fakeClient.AddReactor("get", "replicasets", func(action core.Action) (handled bool, ret runtime.Object, err error) {
		tc.Lock()
		defer tc.Unlock()

		obj := &extensions.Scale{
			ObjectMeta: metav1.ObjectMeta{
				Name:      tc.resource.name,
				Namespace: namespace,
			},
			Spec: extensions.ScaleSpec{
				Replicas: tc.initialReplicas,
			},
			Status: extensions.ScaleStatus{
				Replicas: tc.initialReplicas,
				Selector: selector,
			},
		}
		return true, obj, nil
	})

	fakeClient.AddReactor("list", "pods", func(action core.Action) (handled bool, ret runtime.Object, err error) {
		tc.Lock()
		defer tc.Unlock()

		obj := &v1.PodList{}
		for i := 0; i < len(tc.reportedCPURequests); i++ {
			podReadiness := v1.ConditionTrue
			if tc.reportedPodReadiness != nil {
				podReadiness = tc.reportedPodReadiness[i]
			}
			podName := fmt.Sprintf("%s-%d", podNamePrefix, i)
			pod := v1.Pod{
				Status: v1.PodStatus{
					Phase: v1.PodRunning,
					Conditions: []v1.PodCondition{
						{
							Type:   v1.PodReady,
							Status: podReadiness,
						},
					},
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      podName,
					Namespace: namespace,
					Labels: map[string]string{
						"name": podNamePrefix,
					},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Resources: v1.ResourceRequirements{
								Requests: v1.ResourceList{
									v1.ResourceCPU: tc.reportedCPURequests[i],
								},
							},
						},
					},
				},
			}
			obj.Items = append(obj.Items, pod)
		}
		return true, obj, nil
	})

	fakeClient.AddReactor("update", "replicationcontrollers", func(action core.Action) (handled bool, ret runtime.Object, err error) {
		tc.Lock()
		defer tc.Unlock()

		obj := action.(core.UpdateAction).GetObject().(*extensions.Scale)
		replicas := action.(core.UpdateAction).GetObject().(*extensions.Scale).Spec.Replicas
		assert.Equal(t, tc.desiredReplicas, replicas, "the replica count of the RC should be as expected")
		tc.scaleUpdated = true
		return true, obj, nil
	})

	fakeClient.AddReactor("update", "deployments", func(action core.Action) (handled bool, ret runtime.Object, err error) {
		tc.Lock()
		defer tc.Unlock()

		obj := action.(core.UpdateAction).GetObject().(*extensions.Scale)
		replicas := action.(core.UpdateAction).GetObject().(*extensions.Scale).Spec.Replicas
		assert.Equal(t, tc.desiredReplicas, replicas, "the replica count of the deployment should be as expected")
		tc.scaleUpdated = true
		return true, obj, nil
	})

	fakeClient.AddReactor("update", "replicasets", func(action core.Action) (handled bool, ret runtime.Object, err error) {
		tc.Lock()
		defer tc.Unlock()

		obj := action.(core.UpdateAction).GetObject().(*extensions.Scale)
		replicas := action.(core.UpdateAction).GetObject().(*extensions.Scale).Spec.Replicas
		assert.Equal(t, tc.desiredReplicas, replicas, "the replica count of the replicaset should be as expected")
		tc.scaleUpdated = true
		return true, obj, nil
	})

	fakeClient.AddReactor("update", "horizontalpodautoscalers", func(action core.Action) (handled bool, ret runtime.Object, err error) {
		tc.Lock()
		defer tc.Unlock()

		obj := action.(core.UpdateAction).GetObject().(*autoscalingv1.HorizontalPodAutoscaler)
		assert.Equal(t, namespace, obj.Namespace, "the HPA namespace should be as expected")
		assert.Equal(t, hpaName, obj.Name, "the HPA name should be as expected")
		assert.Equal(t, tc.desiredReplicas, obj.Status.DesiredReplicas, "the desired replica count reported in the object status should be as expected")
		if tc.verifyCPUCurrent {
			assert.NotNil(t, obj.Status.CurrentCPUUtilizationPercentage, "the reported CPU utilization percentage should be non-nil")
			assert.Equal(t, tc.CPUCurrent, *obj.Status.CurrentCPUUtilizationPercentage, "the report CPU utilization percentage should be as expected")
		}
		tc.statusUpdated = true
		// Every time we reconcile HPA object we are updating status.
		tc.processed <- obj.Name
		return true, obj, nil
	})

	fakeWatch := watch.NewFake()
	fakeClient.AddWatchReactor("*", core.DefaultWatchReactor(fakeWatch, nil))

	fakeMetricsClient := &metricsfake.Clientset{}
	// NB: we have to sound like Gollum due to gengo's inability to handle already-plural resource names
	fakeMetricsClient.AddReactor("list", "podmetricses", func(action core.Action) (handled bool, ret runtime.Object, err error) {
		tc.Lock()
		defer tc.Unlock()

		metrics := &metricsapi.PodMetricsList{}
		for i, cpu := range tc.reportedLevels {
			// NB: the list reactor actually does label selector filtering for us,
			// so we have to make sure our results match the label selector
			podMetric := metricsapi.PodMetrics{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-%d", podNamePrefix, i),
					Namespace: namespace,
					Labels:    selector,
				},
				Timestamp: metav1.Time{Time: time.Now()},
				Containers: []metricsapi.ContainerMetrics{
					{
						Name: "container",
						Usage: clientv1.ResourceList{
							clientv1.ResourceCPU: *resource.NewMilliQuantity(
								int64(cpu),
								resource.DecimalSI),
							clientv1.ResourceMemory: *resource.NewQuantity(
								int64(1024*1024),
								resource.BinarySI),
						},
					},
				},
			}
			metrics.Items = append(metrics.Items, podMetric)
		}

		return true, metrics, nil
	})

	fakeCMClient := &cmfake.FakeCustomMetricsClient{}
	fakeCMClient.AddReactor("get", "*", func(action core.Action) (handled bool, ret runtime.Object, err error) {
		tc.Lock()
		defer tc.Unlock()

		getForAction, wasGetFor := action.(cmfake.GetForAction)
		if !wasGetFor {
			return true, nil, fmt.Errorf("expected a get-for action, got %v instead", action)
		}

		if getForAction.GetName() == "*" {
			metrics := &cmapi.MetricValueList{}

			// multiple objects
			assert.Equal(t, "pods", getForAction.GetResource().Resource, "the type of object that we requested multiple metrics for should have been pods")
			assert.Equal(t, "qps", getForAction.GetMetricName(), "the metric name requested should have been qps, as specified in the metric spec")

			for i, level := range tc.reportedLevels {
				podMetric := cmapi.MetricValue{
					DescribedObject: clientv1.ObjectReference{
						Kind:      "Pod",
						Name:      fmt.Sprintf("%s-%d", podNamePrefix, i),
						Namespace: namespace,
					},
					Timestamp:  metav1.Time{Time: time.Now()},
					MetricName: "qps",
					Value:      *resource.NewMilliQuantity(int64(level), resource.DecimalSI),
				}
				metrics.Items = append(metrics.Items, podMetric)
			}

			return true, metrics, nil
		} else {
			name := getForAction.GetName()
			mapper := api.Registry.RESTMapper()
			metrics := &cmapi.MetricValueList{}
			var matchedTarget *autoscalingv2.MetricSpec
			for i, target := range tc.metricsTarget {
				if target.Type == autoscalingv2.ObjectMetricSourceType && name == target.Object.Target.Name {
					gk := schema.FromAPIVersionAndKind(target.Object.Target.APIVersion, target.Object.Target.Kind).GroupKind()
					mapping, err := mapper.RESTMapping(gk)
					if err != nil {
						t.Logf("unable to get mapping for %s: %v", gk.String(), err)
						continue
					}
					groupResource := schema.GroupResource{Group: mapping.GroupVersionKind.Group, Resource: mapping.Resource}

					if getForAction.GetResource().Resource == groupResource.String() {
						matchedTarget = &tc.metricsTarget[i]
					}
				}
			}
			assert.NotNil(t, matchedTarget, "this request should have matched one of the metric specs")
			assert.Equal(t, "qps", getForAction.GetMetricName(), "the metric name requested should have been qps, as specified in the metric spec")

			metrics.Items = []cmapi.MetricValue{
				{
					DescribedObject: clientv1.ObjectReference{
						Kind:       matchedTarget.Object.Target.Kind,
						APIVersion: matchedTarget.Object.Target.APIVersion,
						Name:       name,
					},
					Timestamp:  metav1.Time{Time: time.Now()},
					MetricName: "qps",
					Value:      *resource.NewMilliQuantity(int64(tc.reportedLevels[0]), resource.DecimalSI),
				},
			}

			return true, metrics, nil
		}
	})

	return fakeClient, fakeMetricsClient, fakeCMClient
}

func (tc *testCase) verifyResults(t *testing.T) {
	tc.Lock()
	defer tc.Unlock()

	assert.Equal(t, tc.initialReplicas != tc.desiredReplicas, tc.scaleUpdated, "the scale should only be updated if we expected a change in replicas")
	assert.True(t, tc.statusUpdated, "the status should have been updated")
	if tc.verifyEvents {
		assert.Equal(t, tc.initialReplicas != tc.desiredReplicas, tc.eventCreated, "an event should have been created only if we expected a change in replicas")
	}
}

func (tc *testCase) runTest(t *testing.T) {
	testClient, testMetricsClient, testCMClient := tc.prepareTestClient(t)
	metricsClient := metrics.NewRESTMetricsClient(
		testMetricsClient.MetricsV1alpha1(),
		testCMClient,
	)

	eventClient := &clientfake.Clientset{}
	eventClient.AddReactor("create", "events", func(action core.Action) (handled bool, ret runtime.Object, err error) {
		tc.Lock()
		defer tc.Unlock()

		obj := action.(core.CreateAction).GetObject().(*clientv1.Event)
		if tc.verifyEvents {
			switch obj.Reason {
			case "SuccessfulRescale":
				assert.Equal(t, fmt.Sprintf("New size: %d; reason: cpu resource utilization (percentage of request) above target", tc.desiredReplicas), obj.Message)
			case "DesiredReplicasComputed":
				assert.Equal(t, fmt.Sprintf(
					"Computed the desired num of replicas: %d (avgCPUutil: %d, current replicas: %d)",
					tc.desiredReplicas,
					(int64(tc.reportedLevels[0])*100)/tc.reportedCPURequests[0].MilliValue(), tc.initialReplicas), obj.Message)
			default:
				assert.False(t, true, fmt.Sprintf("Unexpected event: %s / %s", obj.Reason, obj.Message))
			}
		}
		tc.eventCreated = true
		return true, obj, nil
	})

	replicaCalc := &ReplicaCalculator{
		metricsClient: metricsClient,
		podsGetter:    testClient.Core(),
	}

	informerFactory := informers.NewSharedInformerFactory(testClient, controller.NoResyncPeriodFunc())

	hpaController := NewHorizontalController(
		eventClient.Core(),
		testClient.Extensions(),
		testClient.Autoscaling(),
		replicaCalc,
		informerFactory.Autoscaling().V1().HorizontalPodAutoscalers(),
		controller.NoResyncPeriodFunc(),
	)
	hpaController.hpaListerSynced = alwaysReady

	stop := make(chan struct{})
	defer close(stop)
	informerFactory.Start(stop)
	go hpaController.Run(stop)

	tc.Lock()
	if tc.verifyEvents {
		tc.Unlock()
		// We need to wait for events to be broadcasted (sleep for longer than record.sleepDuration).
		time.Sleep(2 * time.Second)
	} else {
		tc.Unlock()
	}
	// Wait for HPA to be processed.
	<-tc.processed
	tc.verifyResults(t)
}

func TestScaleUp(t *testing.T) {
	tc := testCase{
		minReplicas:         2,
		maxReplicas:         6,
		initialReplicas:     3,
		desiredReplicas:     5,
		CPUTarget:           30,
		verifyCPUCurrent:    true,
		reportedLevels:      []uint64{300, 500, 700},
		reportedCPURequests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
		useMetricsApi:       true,
	}
	tc.runTest(t)
}

func TestScaleUpUnreadyLessScale(t *testing.T) {
	tc := testCase{
		minReplicas:          2,
		maxReplicas:          6,
		initialReplicas:      3,
		desiredReplicas:      4,
		CPUTarget:            30,
		CPUCurrent:           60,
		verifyCPUCurrent:     true,
		reportedLevels:       []uint64{300, 500, 700},
		reportedCPURequests:  []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
		reportedPodReadiness: []v1.ConditionStatus{v1.ConditionFalse, v1.ConditionTrue, v1.ConditionTrue},
		useMetricsApi:        true,
	}
	tc.runTest(t)
}

func TestScaleUpUnreadyNoScale(t *testing.T) {
	tc := testCase{
		minReplicas:          2,
		maxReplicas:          6,
		initialReplicas:      3,
		desiredReplicas:      3,
		CPUTarget:            30,
		CPUCurrent:           40,
		verifyCPUCurrent:     true,
		reportedLevels:       []uint64{400, 500, 700},
		reportedCPURequests:  []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
		reportedPodReadiness: []v1.ConditionStatus{v1.ConditionTrue, v1.ConditionFalse, v1.ConditionFalse},
		useMetricsApi:        true,
	}
	tc.runTest(t)
}

func TestScaleUpDeployment(t *testing.T) {
	tc := testCase{
		minReplicas:         2,
		maxReplicas:         6,
		initialReplicas:     3,
		desiredReplicas:     5,
		CPUTarget:           30,
		verifyCPUCurrent:    true,
		reportedLevels:      []uint64{300, 500, 700},
		reportedCPURequests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
		useMetricsApi:       true,
		resource: &fakeResource{
			name:       "test-dep",
			apiVersion: "extensions/v1beta1",
			kind:       "deployments",
		},
	}
	tc.runTest(t)
}

func TestScaleUpReplicaSet(t *testing.T) {
	tc := testCase{
		minReplicas:         2,
		maxReplicas:         6,
		initialReplicas:     3,
		desiredReplicas:     5,
		CPUTarget:           30,
		verifyCPUCurrent:    true,
		reportedLevels:      []uint64{300, 500, 700},
		reportedCPURequests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
		useMetricsApi:       true,
		resource: &fakeResource{
			name:       "test-replicaset",
			apiVersion: "extensions/v1beta1",
			kind:       "replicasets",
		},
	}
	tc.runTest(t)
}

func TestScaleUpCM(t *testing.T) {
	tc := testCase{
		minReplicas:     2,
		maxReplicas:     6,
		initialReplicas: 3,
		desiredReplicas: 4,
		CPUTarget:       0,
		metricsTarget: []autoscalingv2.MetricSpec{
			{
				Type: autoscalingv2.PodsMetricSourceType,
				Pods: &autoscalingv2.PodsMetricSource{
					MetricName:         "qps",
					TargetAverageValue: resource.MustParse("15.0"),
				},
			},
		},
		reportedLevels:      []uint64{20000, 10000, 30000},
		reportedCPURequests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
	}
	tc.runTest(t)
}

func TestScaleUpCMUnreadyLessScale(t *testing.T) {
	tc := testCase{
		minReplicas:     2,
		maxReplicas:     6,
		initialReplicas: 3,
		desiredReplicas: 4,
		CPUTarget:       0,
		metricsTarget: []autoscalingv2.MetricSpec{
			{
				Type: autoscalingv2.PodsMetricSourceType,
				Pods: &autoscalingv2.PodsMetricSource{
					MetricName:         "qps",
					TargetAverageValue: resource.MustParse("15.0"),
				},
			},
		},
		reportedLevels:       []uint64{50000, 10000, 30000},
		reportedPodReadiness: []v1.ConditionStatus{v1.ConditionTrue, v1.ConditionTrue, v1.ConditionFalse},
		reportedCPURequests:  []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
	}
	tc.runTest(t)
}

func TestScaleUpCMUnreadyNoScaleWouldScaleDown(t *testing.T) {
	tc := testCase{
		minReplicas:     2,
		maxReplicas:     6,
		initialReplicas: 3,
		desiredReplicas: 3,
		CPUTarget:       0,
		metricsTarget: []autoscalingv2.MetricSpec{
			{
				Type: autoscalingv2.PodsMetricSourceType,
				Pods: &autoscalingv2.PodsMetricSource{
					MetricName:         "qps",
					TargetAverageValue: resource.MustParse("15.0"),
				},
			},
		},
		reportedLevels:       []uint64{50000, 15000, 30000},
		reportedPodReadiness: []v1.ConditionStatus{v1.ConditionFalse, v1.ConditionTrue, v1.ConditionFalse},
		reportedCPURequests:  []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
	}
	tc.runTest(t)
}

func TestScaleUpCMObject(t *testing.T) {
	tc := testCase{
		minReplicas:     2,
		maxReplicas:     6,
		initialReplicas: 3,
		desiredReplicas: 4,
		CPUTarget:       0,
		metricsTarget: []autoscalingv2.MetricSpec{
			{
				Type: autoscalingv2.ObjectMetricSourceType,
				Object: &autoscalingv2.ObjectMetricSource{
					Target: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "extensions/v1beta1",
						Kind:       "Deployment",
						Name:       "some-deployment",
					},
					MetricName:  "qps",
					TargetValue: resource.MustParse("15.0"),
				},
			},
		},
		reportedLevels: []uint64{20000},
	}
	tc.runTest(t)
}

func TestScaleDown(t *testing.T) {
	tc := testCase{
		minReplicas:         2,
		maxReplicas:         6,
		initialReplicas:     5,
		desiredReplicas:     3,
		CPUTarget:           50,
		verifyCPUCurrent:    true,
		reportedLevels:      []uint64{100, 300, 500, 250, 250},
		reportedCPURequests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
		useMetricsApi:       true,
	}
	tc.runTest(t)
}

func TestScaleDownCM(t *testing.T) {
	tc := testCase{
		minReplicas:     2,
		maxReplicas:     6,
		initialReplicas: 5,
		desiredReplicas: 3,
		CPUTarget:       0,
		metricsTarget: []autoscalingv2.MetricSpec{
			{
				Type: autoscalingv2.PodsMetricSourceType,
				Pods: &autoscalingv2.PodsMetricSource{
					MetricName:         "qps",
					TargetAverageValue: resource.MustParse("20.0"),
				},
			},
		},
		reportedLevels:      []uint64{12000, 12000, 12000, 12000, 12000},
		reportedCPURequests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
	}
	tc.runTest(t)
}

func TestScaleDownCMObject(t *testing.T) {
	tc := testCase{
		minReplicas:     2,
		maxReplicas:     6,
		initialReplicas: 5,
		desiredReplicas: 3,
		CPUTarget:       0,
		metricsTarget: []autoscalingv2.MetricSpec{
			{
				Type: autoscalingv2.ObjectMetricSourceType,
				Object: &autoscalingv2.ObjectMetricSource{
					Target: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "extensions/v1beta1",
						Kind:       "Deployment",
						Name:       "some-deployment",
					},
					MetricName:  "qps",
					TargetValue: resource.MustParse("20.0"),
				},
			},
		},
		reportedLevels:      []uint64{12000},
		reportedCPURequests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
	}
	tc.runTest(t)
}

func TestScaleDownIgnoresUnreadyPods(t *testing.T) {
	tc := testCase{
		minReplicas:          2,
		maxReplicas:          6,
		initialReplicas:      5,
		desiredReplicas:      2,
		CPUTarget:            50,
		CPUCurrent:           30,
		verifyCPUCurrent:     true,
		reportedLevels:       []uint64{100, 300, 500, 250, 250},
		reportedCPURequests:  []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
		useMetricsApi:        true,
		reportedPodReadiness: []v1.ConditionStatus{v1.ConditionTrue, v1.ConditionTrue, v1.ConditionTrue, v1.ConditionFalse, v1.ConditionFalse},
	}
	tc.runTest(t)
}

func TestTolerance(t *testing.T) {
	tc := testCase{
		minReplicas:         1,
		maxReplicas:         5,
		initialReplicas:     3,
		desiredReplicas:     3,
		CPUTarget:           100,
		reportedLevels:      []uint64{1010, 1030, 1020},
		reportedCPURequests: []resource.Quantity{resource.MustParse("0.9"), resource.MustParse("1.0"), resource.MustParse("1.1")},
		useMetricsApi:       true,
	}
	tc.runTest(t)
}

func TestToleranceCM(t *testing.T) {
	tc := testCase{
		minReplicas:     1,
		maxReplicas:     5,
		initialReplicas: 3,
		desiredReplicas: 3,
		metricsTarget: []autoscalingv2.MetricSpec{
			{
				Type: autoscalingv2.PodsMetricSourceType,
				Pods: &autoscalingv2.PodsMetricSource{
					MetricName:         "qps",
					TargetAverageValue: resource.MustParse("20.0"),
				},
			},
		},
		reportedLevels:      []uint64{20000, 20001, 21000},
		reportedCPURequests: []resource.Quantity{resource.MustParse("0.9"), resource.MustParse("1.0"), resource.MustParse("1.1")},
	}
	tc.runTest(t)
}

func TestToleranceCMObject(t *testing.T) {
	tc := testCase{
		minReplicas:     1,
		maxReplicas:     5,
		initialReplicas: 3,
		desiredReplicas: 3,
		metricsTarget: []autoscalingv2.MetricSpec{
			{
				Type: autoscalingv2.ObjectMetricSourceType,
				Object: &autoscalingv2.ObjectMetricSource{
					Target: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "extensions/v1beta1",
						Kind:       "Deployment",
						Name:       "some-deployment",
					},
					MetricName:  "qps",
					TargetValue: resource.MustParse("20.0"),
				},
			},
		},
		reportedLevels:      []uint64{20050},
		reportedCPURequests: []resource.Quantity{resource.MustParse("0.9"), resource.MustParse("1.0"), resource.MustParse("1.1")},
	}
	tc.runTest(t)
}

func TestMinReplicas(t *testing.T) {
	tc := testCase{
		minReplicas:         2,
		maxReplicas:         5,
		initialReplicas:     3,
		desiredReplicas:     2,
		CPUTarget:           90,
		reportedLevels:      []uint64{10, 95, 10},
		reportedCPURequests: []resource.Quantity{resource.MustParse("0.9"), resource.MustParse("1.0"), resource.MustParse("1.1")},
		useMetricsApi:       true,
	}
	tc.runTest(t)
}

func TestZeroReplicas(t *testing.T) {
	tc := testCase{
		minReplicas:         3,
		maxReplicas:         5,
		initialReplicas:     0,
		desiredReplicas:     0,
		CPUTarget:           90,
		reportedLevels:      []uint64{},
		reportedCPURequests: []resource.Quantity{},
		useMetricsApi:       true,
	}
	tc.runTest(t)
}

func TestTooFewReplicas(t *testing.T) {
	tc := testCase{
		minReplicas:         3,
		maxReplicas:         5,
		initialReplicas:     2,
		desiredReplicas:     3,
		CPUTarget:           90,
		reportedLevels:      []uint64{},
		reportedCPURequests: []resource.Quantity{},
		useMetricsApi:       true,
	}
	tc.runTest(t)
}

func TestTooManyReplicas(t *testing.T) {
	tc := testCase{
		minReplicas:         3,
		maxReplicas:         5,
		initialReplicas:     10,
		desiredReplicas:     5,
		CPUTarget:           90,
		reportedLevels:      []uint64{},
		reportedCPURequests: []resource.Quantity{},
		useMetricsApi:       true,
	}
	tc.runTest(t)
}

func TestMaxReplicas(t *testing.T) {
	tc := testCase{
		minReplicas:         2,
		maxReplicas:         5,
		initialReplicas:     3,
		desiredReplicas:     5,
		CPUTarget:           90,
		reportedLevels:      []uint64{8000, 9500, 1000},
		reportedCPURequests: []resource.Quantity{resource.MustParse("0.9"), resource.MustParse("1.0"), resource.MustParse("1.1")},
		useMetricsApi:       true,
	}
	tc.runTest(t)
}

func TestSuperfluousMetrics(t *testing.T) {
	tc := testCase{
		minReplicas:         2,
		maxReplicas:         6,
		initialReplicas:     4,
		desiredReplicas:     6,
		CPUTarget:           100,
		reportedLevels:      []uint64{4000, 9500, 3000, 7000, 3200, 2000},
		reportedCPURequests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
		useMetricsApi:       true,
	}
	tc.runTest(t)
}

func TestMissingMetrics(t *testing.T) {
	tc := testCase{
		minReplicas:         2,
		maxReplicas:         6,
		initialReplicas:     4,
		desiredReplicas:     3,
		CPUTarget:           100,
		reportedLevels:      []uint64{400, 95},
		reportedCPURequests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
		useMetricsApi:       true,
	}
	tc.runTest(t)
}

func TestEmptyMetrics(t *testing.T) {
	tc := testCase{
		minReplicas:         2,
		maxReplicas:         6,
		initialReplicas:     4,
		desiredReplicas:     4,
		CPUTarget:           100,
		reportedLevels:      []uint64{},
		reportedCPURequests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
		useMetricsApi:       true,
	}
	tc.runTest(t)
}

func TestEmptyCPURequest(t *testing.T) {
	tc := testCase{
		minReplicas:     1,
		maxReplicas:     5,
		initialReplicas: 1,
		desiredReplicas: 1,
		CPUTarget:       100,
		reportedLevels:  []uint64{200},
		useMetricsApi:   true,
	}
	tc.runTest(t)
}

func TestEventCreated(t *testing.T) {
	tc := testCase{
		minReplicas:         1,
		maxReplicas:         5,
		initialReplicas:     1,
		desiredReplicas:     2,
		CPUTarget:           50,
		reportedLevels:      []uint64{200},
		reportedCPURequests: []resource.Quantity{resource.MustParse("0.2")},
		verifyEvents:        true,
		useMetricsApi:       true,
	}
	tc.runTest(t)
}

func TestEventNotCreated(t *testing.T) {
	tc := testCase{
		minReplicas:         1,
		maxReplicas:         5,
		initialReplicas:     2,
		desiredReplicas:     2,
		CPUTarget:           50,
		reportedLevels:      []uint64{200, 200},
		reportedCPURequests: []resource.Quantity{resource.MustParse("0.4"), resource.MustParse("0.4")},
		verifyEvents:        true,
		useMetricsApi:       true,
	}
	tc.runTest(t)
}

func TestMissingReports(t *testing.T) {
	tc := testCase{
		minReplicas:         1,
		maxReplicas:         5,
		initialReplicas:     4,
		desiredReplicas:     2,
		CPUTarget:           50,
		reportedLevels:      []uint64{200},
		reportedCPURequests: []resource.Quantity{resource.MustParse("0.2")},
		useMetricsApi:       true,
	}
	tc.runTest(t)
}

func TestUpscaleCap(t *testing.T) {
	tc := testCase{
		minReplicas:         1,
		maxReplicas:         100,
		initialReplicas:     3,
		desiredReplicas:     6,
		CPUTarget:           10,
		reportedLevels:      []uint64{100, 200, 300},
		reportedCPURequests: []resource.Quantity{resource.MustParse("0.1"), resource.MustParse("0.1"), resource.MustParse("0.1")},
		useMetricsApi:       true,
	}
	tc.runTest(t)
}

// TestComputedToleranceAlgImplementation is a regression test which
// back-calculates a minimal percentage for downscaling based on a small percentage
// increase in pod utilization which is calibrated against the tolerance value.
func TestComputedToleranceAlgImplementation(t *testing.T) {

	startPods := int32(10)
	// 150 mCPU per pod.
	totalUsedCPUOfAllPods := uint64(startPods * 150)
	// Each pod starts out asking for 2X what is really needed.
	// This means we will have a 50% ratio of used/requested
	totalRequestedCPUOfAllPods := int32(2 * totalUsedCPUOfAllPods)
	requestedToUsed := float64(totalRequestedCPUOfAllPods / int32(totalUsedCPUOfAllPods))
	// Spread the amount we ask over 10 pods.  We can add some jitter later in reportedLevels.
	perPodRequested := totalRequestedCPUOfAllPods / startPods

	// Force a minimal scaling event by satisfying  (tolerance < 1 - resourcesUsedRatio).
	target := math.Abs(1/(requestedToUsed*(1-tolerance))) + .01
	finalCpuPercentTarget := int32(target * 100)
	resourcesUsedRatio := float64(totalUsedCPUOfAllPods) / float64(float64(totalRequestedCPUOfAllPods)*target)

	// i.e. .60 * 20 -> scaled down expectation.
	finalPods := int32(math.Ceil(resourcesUsedRatio * float64(startPods)))

	// To breach tolerance we will create a utilization ratio difference of tolerance to usageRatioToleranceValue)
	tc := testCase{
		minReplicas:     0,
		maxReplicas:     1000,
		initialReplicas: startPods,
		desiredReplicas: finalPods,
		CPUTarget:       finalCpuPercentTarget,
		reportedLevels: []uint64{
			totalUsedCPUOfAllPods / 10,
			totalUsedCPUOfAllPods / 10,
			totalUsedCPUOfAllPods / 10,
			totalUsedCPUOfAllPods / 10,
			totalUsedCPUOfAllPods / 10,
			totalUsedCPUOfAllPods / 10,
			totalUsedCPUOfAllPods / 10,
			totalUsedCPUOfAllPods / 10,
			totalUsedCPUOfAllPods / 10,
			totalUsedCPUOfAllPods / 10,
		},
		reportedCPURequests: []resource.Quantity{
			resource.MustParse(fmt.Sprint(perPodRequested+100) + "m"),
			resource.MustParse(fmt.Sprint(perPodRequested-100) + "m"),
			resource.MustParse(fmt.Sprint(perPodRequested+10) + "m"),
			resource.MustParse(fmt.Sprint(perPodRequested-10) + "m"),
			resource.MustParse(fmt.Sprint(perPodRequested+2) + "m"),
			resource.MustParse(fmt.Sprint(perPodRequested-2) + "m"),
			resource.MustParse(fmt.Sprint(perPodRequested+1) + "m"),
			resource.MustParse(fmt.Sprint(perPodRequested-1) + "m"),
			resource.MustParse(fmt.Sprint(perPodRequested) + "m"),
			resource.MustParse(fmt.Sprint(perPodRequested) + "m"),
		},
		useMetricsApi: true,
	}

	tc.runTest(t)

	// Reuse the data structure above, now testing "unscaling".
	// Now, we test that no scaling happens if we are in a very close margin to the tolerance
	target = math.Abs(1/(requestedToUsed*(1-tolerance))) + .004
	finalCpuPercentTarget = int32(target * 100)
	tc.CPUTarget = finalCpuPercentTarget
	tc.initialReplicas = startPods
	tc.desiredReplicas = startPods
	tc.runTest(t)
}

func TestScaleUpRCImmediately(t *testing.T) {
	time := metav1.Time{Time: time.Now()}
	tc := testCase{
		minReplicas:         2,
		maxReplicas:         6,
		initialReplicas:     1,
		desiredReplicas:     2,
		verifyCPUCurrent:    false,
		reportedLevels:      []uint64{0, 0, 0, 0},
		reportedCPURequests: []resource.Quantity{resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0"), resource.MustParse("1.0")},
		useMetricsApi:       true,
		lastScaleTime:       &time,
	}
	tc.runTest(t)
}

func TestScaleDownRCImmediately(t *testing.T) {
	time := metav1.Time{Time: time.Now()}
	tc := testCase{
		minReplicas:         2,
		maxReplicas:         5,
		initialReplicas:     6,
		desiredReplicas:     5,
		CPUTarget:           50,
		reportedLevels:      []uint64{8000, 9500, 1000},
		reportedCPURequests: []resource.Quantity{resource.MustParse("0.9"), resource.MustParse("1.0"), resource.MustParse("1.1")},
		useMetricsApi:       true,
		lastScaleTime:       &time,
	}
	tc.runTest(t)
}

// TODO: add more tests