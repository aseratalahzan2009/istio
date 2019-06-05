// Copyright 2019 Istio Authors
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

package sidecarscope

import (
	"reflect"
	"testing"
	"time"

	xdsapi "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	xdscore "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	"github.com/gogo/protobuf/proto"

	v2 "istio.io/istio/pilot/pkg/proxy/envoy/v2"

	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/scopes"
)

func TestServiceEntryDNS(t *testing.T) {
	framework.Run(t, func(ctx framework.TestContext) {
		config := Config{
			Resolution: "DNS",
		}
		p, nodeID := setupTest(t, ctx, config)

		req := &xdsapi.DiscoveryRequest{
			Node: &xdscore.Node{
				Id: nodeID.ServiceNode(),
			},
			TypeUrl: v2.ClusterType,
		}

		if err := p.StartDiscovery(req); err != nil {
			t.Fatal(err)
		}
		if err := p.WatchDiscovery(time.Second*5, checkResultDNS); err != nil {
			t.Fatal(err)
		}
	})
}

func checkResultDNS(resp *xdsapi.DiscoveryResponse) (success bool, e error) {
	expected := map[string]int{
		"included.com": 1,
		"app.com":      1,
	}

	found := false
	for _, res := range resp.Resources {
		c := &xdsapi.Cluster{}
		if err := proto.Unmarshal(res.Value, c); err != nil {
			return false, err
		}
		if c.Name != "outbound|80||app.com" {
			continue
		}
		found = true

		got := map[string]int{}
		for _, ep := range c.LoadAssignment.Endpoints {
			for _, lb := range ep.LbEndpoints {
				got[lb.GetEndpoint().Address.GetSocketAddress().Address]++
			}
		}
		if !reflect.DeepEqual(expected, got) {
			scopes.Framework.Warnf("Excepted load assignments %+v, got %+v. Continuing in case pilot updates with correct response", expected, got)
			return false, nil
		}
	}
	if !found {
		// We didn't find the expected cluster, keep watching
		return false, nil
	}
	return true, nil
}
