/*
Copyright 2014 The Kubernetes Authors.

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

package storage

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/generic"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"github.com/sourcegraph/monorepo-test-1/kubernetes-5/pkg/api"
	"github.com/sourcegraph/monorepo-test-1/kubernetes-5/pkg/registry/cachesize"
	"github.com/sourcegraph/monorepo-test-1/kubernetes-5/pkg/registry/core/podtemplate"
)

type REST struct {
	*genericregistry.Store
}

// NewREST returns a RESTStorage object that will work against pod templates.
func NewREST(optsGetter generic.RESTOptionsGetter) *REST {
	store := &genericregistry.Store{
		Copier:      api.Scheme,
		NewFunc:     func() runtime.Object { return &api.PodTemplate{} },
		NewListFunc: func() runtime.Object { return &api.PodTemplateList{} },
		ObjectNameFunc: func(obj runtime.Object) (string, error) {
			return obj.(*api.PodTemplate).Name, nil
		},
		PredicateFunc:     podtemplate.MatchPodTemplate,
		QualifiedResource: api.Resource("podtemplates"),
		WatchCacheSize:    cachesize.GetWatchCacheSizeByResource("podtemplates"),

		CreateStrategy: podtemplate.Strategy,
		UpdateStrategy: podtemplate.Strategy,
		DeleteStrategy: podtemplate.Strategy,
		ExportStrategy: podtemplate.Strategy,

		ReturnDeletedObject: true,
	}
	options := &generic.StoreOptions{RESTOptions: optsGetter, AttrFunc: podtemplate.GetAttrs}
	if err := store.CompleteWithOptions(options); err != nil {
		panic(err) // TODO: Propagate error up
	}
	return &REST{store}
}
