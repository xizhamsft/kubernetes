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

package priorities

import (
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	framework "k8s.io/kubernetes/pkg/scheduler/framework/v1alpha1"
	schedulerlisters "k8s.io/kubernetes/pkg/scheduler/listers"
	schedulernodeinfo "k8s.io/kubernetes/pkg/scheduler/nodeinfo"
	utilnode "k8s.io/kubernetes/pkg/util/node"

	"k8s.io/klog"
)

// When zone information is present, give 2/3 of the weighting to zone spreading, 1/3 to node spreading
// TODO: Any way to justify this weighting?
const zoneWeighting float64 = 2.0 / 3.0

// SelectorSpread contains information to calculate selector spread priority.
type SelectorSpread struct {
	serviceLister     corelisters.ServiceLister
	controllerLister  corelisters.ReplicationControllerLister
	replicaSetLister  appslisters.ReplicaSetLister
	statefulSetLister appslisters.StatefulSetLister
}

// NewSelectorSpreadPriority creates a SelectorSpread.
func NewSelectorSpreadPriority(
	serviceLister corelisters.ServiceLister,
	controllerLister corelisters.ReplicationControllerLister,
	replicaSetLister appslisters.ReplicaSetLister,
	statefulSetLister appslisters.StatefulSetLister) (PriorityMapFunction, PriorityReduceFunction) {
	selectorSpread := &SelectorSpread{
		serviceLister:     serviceLister,
		controllerLister:  controllerLister,
		replicaSetLister:  replicaSetLister,
		statefulSetLister: statefulSetLister,
	}
	return selectorSpread.CalculateSpreadPriorityMap, selectorSpread.CalculateSpreadPriorityReduce
}

// CalculateSpreadPriorityMap spreads pods across hosts, considering pods
// belonging to the same service,RC,RS or StatefulSet.
// When a pod is scheduled, it looks for services, RCs,RSs and StatefulSets that match the pod,
// then finds existing pods that match those selectors.
// It favors nodes that have fewer existing matching pods.
// i.e. it pushes the scheduler towards a node where there's the smallest number of
// pods which match the same service, RC,RSs or StatefulSets selectors as the pod being scheduled.
func (s *SelectorSpread) CalculateSpreadPriorityMap(pod *v1.Pod, meta interface{}, nodeInfo *schedulernodeinfo.NodeInfo) (framework.NodeScore, error) {
	var selector labels.Selector
	node := nodeInfo.Node()
	if node == nil {
		return framework.NodeScore{}, fmt.Errorf("node not found")
	}

	priorityMeta, ok := meta.(*priorityMetadata)
	if ok {
		selector = priorityMeta.podSelector
	} else {
		selector = getSelector(pod, s.serviceLister, s.controllerLister, s.replicaSetLister, s.statefulSetLister)
	}

	count := countMatchingPods(pod.Namespace, selector, nodeInfo)
	return framework.NodeScore{
		Name:  node.Name,
		Score: int64(count),
	}, nil
}

// CalculateSpreadPriorityReduce calculates the source of each node
// based on the number of existing matching pods on the node
// where zone information is included on the nodes, it favors nodes
// in zones with fewer existing matching pods.
func (s *SelectorSpread) CalculateSpreadPriorityReduce(pod *v1.Pod, meta interface{}, sharedLister schedulerlisters.SharedLister, result framework.NodeScoreList) error {
	countsByZone := make(map[string]int64, 10)
	maxCountByZone := int64(0)
	maxCountByNodeName := int64(0)

	for i := range result {
		if result[i].Score > maxCountByNodeName {
			maxCountByNodeName = result[i].Score
		}
		nodeInfo, err := sharedLister.NodeInfos().Get(result[i].Name)
		if err != nil {
			return err
		}
		zoneID := utilnode.GetZoneKey(nodeInfo.Node())
		if zoneID == "" {
			continue
		}
		countsByZone[zoneID] += result[i].Score
	}

	for zoneID := range countsByZone {
		if countsByZone[zoneID] > maxCountByZone {
			maxCountByZone = countsByZone[zoneID]
		}
	}

	haveZones := len(countsByZone) != 0

	maxCountByNodeNameFloat64 := float64(maxCountByNodeName)
	maxCountByZoneFloat64 := float64(maxCountByZone)
	MaxNodeScoreFloat64 := float64(framework.MaxNodeScore)

	for i := range result {
		// initializing to the default/max node score of maxPriority
		fScore := MaxNodeScoreFloat64
		if maxCountByNodeName > 0 {
			fScore = MaxNodeScoreFloat64 * (float64(maxCountByNodeName-result[i].Score) / maxCountByNodeNameFloat64)
		}
		// If there is zone information present, incorporate it
		if haveZones {
			nodeInfo, err := sharedLister.NodeInfos().Get(result[i].Name)
			if err != nil {
				return err
			}

			zoneID := utilnode.GetZoneKey(nodeInfo.Node())
			if zoneID != "" {
				zoneScore := MaxNodeScoreFloat64
				if maxCountByZone > 0 {
					zoneScore = MaxNodeScoreFloat64 * (float64(maxCountByZone-countsByZone[zoneID]) / maxCountByZoneFloat64)
				}
				fScore = (fScore * (1.0 - zoneWeighting)) + (zoneWeighting * zoneScore)
			}
		}
		result[i].Score = int64(fScore)
		if klog.V(10) {
			klog.Infof(
				"%v -> %v: SelectorSpreadPriority, Score: (%d)", pod.Name, result[i].Name, int64(fScore),
			)
		}
	}
	return nil
}

// countMatchingPods counts pods based on namespace and matching all selectors
func countMatchingPods(namespace string, selector labels.Selector, nodeInfo *schedulernodeinfo.NodeInfo) int {
	if nodeInfo.Pods() == nil || len(nodeInfo.Pods()) == 0 || selector.Empty() {
		return 0
	}
	count := 0
	for _, pod := range nodeInfo.Pods() {
		// Ignore pods being deleted for spreading purposes
		// Similar to how it is done for SelectorSpreadPriority
		if namespace == pod.Namespace && pod.DeletionTimestamp == nil {
			if selector.Matches(labels.Set(pod.Labels)) {
				count++
			}
		}
	}
	return count
}
