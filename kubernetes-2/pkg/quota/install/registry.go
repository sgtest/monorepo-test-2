/*
Copyright 2016 The Kubernetes Authors.

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

package install

import (
	"github.com/sourcegraph/monorepo-test-1/kubernetes-2/pkg/client/clientset_generated/clientset"
	informers "github.com/sourcegraph/monorepo-test-1/kubernetes-2/pkg/client/informers/informers_generated/externalversions"
	"github.com/sourcegraph/monorepo-test-1/kubernetes-2/pkg/quota"
	"github.com/sourcegraph/monorepo-test-1/kubernetes-2/pkg/quota/evaluator/core"
)

// NewRegistry returns a registry of quota evaluators.
// If a shared informer factory is provided, it is used by evaluators rather than performing direct queries.
func NewRegistry(kubeClient clientset.Interface, f informers.SharedInformerFactory) quota.Registry {
	// TODO: when quota supports resources in other api groups, we will need to merge
	return core.NewRegistry(kubeClient, f)
}
