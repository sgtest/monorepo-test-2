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

package main

import (
	"github.com/sourcegraph/monorepo-test-1/kubernetes-15/federation/cmd/federation-controller-manager/app"
	"github.com/sourcegraph/monorepo-test-1/kubernetes-15/federation/cmd/federation-controller-manager/app/options"
)

// NewFederationCMServer creates a new hyperkube Server object that includes the
// description and flags.
func NewFederationCMServer() *Server {
	s := options.NewCMServer()

	hks := Server{
		SimpleUsage: "federation-controller-manager",
		Long:        "Controller manager for federation control plane. Manages federation service endpoints and controllers",
		Run: func(_ *Server, args []string) error {
			return app.Run(s)
		},
	}
	s.AddFlags(hks.Flags())
	return &hks
}
