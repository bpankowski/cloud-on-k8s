// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package elasticsearch

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	estype "github.com/elastic/cloud-on-k8s/pkg/apis/elasticsearch/v1beta1"
	"github.com/elastic/cloud-on-k8s/pkg/controller/common/certificates"
	"github.com/elastic/cloud-on-k8s/pkg/controller/common/hash"
	"github.com/elastic/cloud-on-k8s/pkg/controller/elasticsearch/label"
	"github.com/elastic/cloud-on-k8s/pkg/controller/elasticsearch/sset"
	"github.com/elastic/cloud-on-k8s/pkg/utils/k8s"
	"github.com/elastic/cloud-on-k8s/test/e2e/test"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// BuilderHashAnnotation is the name of an annotation set by the E2E tests on Elasticsearch resources
// containing the hash of their Builder, for comparison purposes (pre/post rolling upgrade).
const BuilderHashAnnotation = "elasticsearch.k8s.elastic.co/e2e-builder-hash"

func (b Builder) CheckK8sTestSteps(k *test.K8sClient) test.StepList {
	return test.StepList{
		CheckCertificateAuthority(b, k),
		CheckExpectedPodsEventuallyReady(b, k),
		CheckESVersion(b, k),
		CheckServices(b, k),
		CheckPodCertificates(b, k),
		CheckServicesEndpoints(b, k),
		CheckClusterHealth(b, k),
		CheckESPassword(b, k),
		CheckESDataVolumeType(b.Elasticsearch, k),
	}
}

// CheckCertificateAuthority checks that the CA is fully setup (CA cert + private key)
func CheckCertificateAuthority(b Builder, k *test.K8sClient) test.Step {
	return test.Step{
		Name: "ES certificate authority should be set and deployed",
		Test: test.Eventually(func() error {
			// Check that the Transport CA may be loaded
			_, err := k.GetCA(b.Elasticsearch.Namespace, b.Elasticsearch.Name, certificates.TransportCAType)
			if err != nil {
				return err
			}

			// Check that the HTTP CA may be loaded
			_, err = k.GetCA(b.Elasticsearch.Namespace, b.Elasticsearch.Name, certificates.HTTPCAType)
			if err != nil {
				return err
			}

			return nil
		}),
	}
}

// CheckPodCertificates checks that all pods have a private key and signed certificate
func CheckPodCertificates(b Builder, k *test.K8sClient) test.Step {
	return test.Step{
		Name: "ES pods should eventually have a certificate",
		Test: test.Eventually(func() error {
			pods, err := k.GetPods(test.ESPodListOptions(b.Elasticsearch.Namespace, b.Elasticsearch.Name)...)
			if err != nil {
				return err
			}
			for _, pod := range pods {
				_, _, err := k.GetTransportCert(b.Elasticsearch.Name, pod.Name)
				if err != nil {
					return err
				}
			}
			return nil
		}),
	}
}

// CheckESPodsRunning checks that all ES pods for the given ES are running
func CheckESPodsPending(b Builder, k *test.K8sClient) test.Step {
	return checkESPodsPhase(b, k, corev1.PodPending)
}

func checkESPodsPhase(b Builder, k *test.K8sClient, phase corev1.PodPhase) test.Step {
	return CheckPodsCondition(b,
		k,
		fmt.Sprintf("Pods should eventually be %s", phase),
		func(p corev1.Pod) error {
			if p.Status.Phase != phase {
				return fmt.Errorf("pod not %s", phase)
			}
			return nil
		},
	)
}

func CheckPodsCondition(b Builder, k *test.K8sClient, name string, condition func(p corev1.Pod) error) test.Step {
	return test.Step{
		Name: name,
		Test: test.Eventually(func() error {
			pods, err := k.GetPods(test.ESPodListOptions(b.Elasticsearch.Namespace, b.Elasticsearch.Name)...)
			if err != nil {
				return err
			}
			if int32(len(pods)) != b.Elasticsearch.Spec.NodeCount() {
				return fmt.Errorf("expected %d pods, got %d", len(pods), b.Elasticsearch.Spec.NodeCount())
			}
			return test.OnAllPods(pods, condition)
		}),
	}
}

// CheckESVersion checks that the running ES version is the expected one
func CheckESVersion(b Builder, k *test.K8sClient) test.Step {
	return test.Step{
		Name: "ES version should be the expected one",
		Test: test.Eventually(func() error {
			pods, err := k.GetPods(test.ESPodListOptions(b.Elasticsearch.Namespace, b.Elasticsearch.Name)...)
			if err != nil {
				return err
			}
			// check number of pods
			if len(pods) != int(b.Elasticsearch.Spec.NodeCount()) {
				return fmt.Errorf("expected %d pods, got %d", b.Elasticsearch.Spec.NodeCount(), len(pods))
			}
			// check ES version label
			for _, p := range pods {
				version := p.Labels[label.VersionLabelName]
				if version != b.Elasticsearch.Spec.Version {
					return fmt.Errorf("version %s does not match expected version %s", version, b.Elasticsearch.Spec.Version)
				}
			}
			return nil
		}),
	}
}

// CheckClusterHealth checks that the given ES status reports a green ES health
func CheckClusterHealth(b Builder, k *test.K8sClient) test.Step {
	return test.Step{
		Name: "ES cluster health should eventually be green",
		Test: test.Eventually(func() error {
			return clusterHealthGreen(b, k)
		}),
	}
}

func clusterHealthGreen(b Builder, k *test.K8sClient) error {
	var es estype.Elasticsearch
	err := k.Client.Get(k8s.ExtractNamespacedName(&b.Elasticsearch), &es)
	if err != nil {
		return err
	}
	if es.Status.Health != estype.ElasticsearchGreenHealth {
		return fmt.Errorf("health is %s", es.Status.Health)
	}
	return nil
}

// CheckServices checks that all ES services are created
func CheckServices(b Builder, k *test.K8sClient) test.Step {
	return test.Step{
		Name: "ES services should be created",
		Test: test.Eventually(func() error {
			for _, s := range []string{
				estype.HTTPService(b.Elasticsearch.Name),
			} {
				if _, err := k.GetService(b.Elasticsearch.Namespace, s); err != nil {
					return err
				}
			}
			return nil
		}),
	}
}

// CheckServicesEndpoints checks that services have the expected number of endpoints
func CheckServicesEndpoints(b Builder, k *test.K8sClient) test.Step {
	return test.Step{
		Name: "ES services should have endpoints",
		Test: test.Eventually(func() error {
			for endpointName, addrCount := range map[string]int{
				estype.HTTPService(b.Elasticsearch.Name): int(b.Elasticsearch.Spec.NodeCount()),
			} {
				if addrCount == 0 {
					continue // maybe no Kibana
				}
				endpoints, err := k.GetEndpoints(b.Elasticsearch.Namespace, endpointName)
				if err != nil {
					return err
				}
				if len(endpoints.Subsets) == 0 {
					return fmt.Errorf("no subset for endpoint %s", endpointName)
				}
				if len(endpoints.Subsets[0].Addresses) != addrCount {
					return fmt.Errorf("%d addresses found for endpoint %s, expected %d", len(endpoints.Subsets[0].Addresses), endpointName, addrCount)
				}
			}
			return nil
		}),
	}
}

// CheckESPassword checks that the user password to access ES is correctly set
func CheckESPassword(b Builder, k *test.K8sClient) test.Step {
	return test.Step{
		Name: "Elastic password should be available",
		Test: test.Eventually(func() error {
			password, err := k.GetElasticPassword(b.Elasticsearch.Namespace, b.Elasticsearch.Name)
			if err != nil {
				return err
			}
			if password == "" {
				return fmt.Errorf("user password is not set")
			}
			return nil
		}),
	}
}

func CheckExpectedPodsEventuallyReady(b Builder, k *test.K8sClient) test.Step {
	return test.Step{
		Name: "All expected Pods should eventually be ready",
		Test: test.Eventually(func() error {
			return checkExpectedPodsReady(b, k)
		}),
	}
}

// checkExpectedPodsReady checks that all expected Pods (no more, no less) are there, ready,
// and that any rolling upgrade is over.
// It does not check the entire spec of the Pods.
func checkExpectedPodsReady(b Builder, k *test.K8sClient) error {
	// check StatefulSets are expected
	if err := checkStatefulSetsReplicas(b, k); err != nil {
		return err
	}
	// for each StatefulSet, make sure all Pods are there and Ready
	for _, nodeSet := range b.Elasticsearch.Spec.NodeSets {
		// retrieve the corresponding StatefulSet
		var statefulSet appsv1.StatefulSet
		if err := k.Client.Get(
			types.NamespacedName{
				Namespace: b.Elasticsearch.Namespace,
				Name:      estype.StatefulSet(b.Elasticsearch.Name, nodeSet.Name),
			},
			&statefulSet,
		); err != nil {
			return err
		}
		// the exact expected list of Pods (no more, no less) should exist
		expectedPodNames := sset.PodNames(statefulSet)
		actualPods, err := sset.GetActualPodsForStatefulSet(k.Client, k8s.ExtractNamespacedName(&statefulSet))
		if err != nil {
			return err
		}
		actualPodNames := make([]string, 0, len(actualPods))
		for _, p := range actualPods {
			actualPodNames = append(actualPodNames, p.Name)
		}
		// sort alphabetically for comparison purposes
		sort.Strings(expectedPodNames)
		sort.Strings(actualPodNames)
		if !reflect.DeepEqual(expectedPodNames, actualPodNames) {
			return fmt.Errorf("invalid Pods for StatefulSet %s: expected %v, got %v", statefulSet.Name, expectedPodNames, actualPodNames)
		}

		// all Pods should be running and ready
		for _, p := range actualPods {
			if !k8s.IsPodReady(p) {
				// pretty-print status JSON
				statusJSON, err := json.MarshalIndent(p.Status, "", "    ")
				if err != nil {
					return err
				}
				return fmt.Errorf("pod %s is not Ready.\nStatus:%s", p.Name, statusJSON)
			}
			// Pod should either:
			// - be annotated with the hash of the current ES spec from previous E2E steps
			// - not be annotated at all (if recreated/upgraded, or not a mutation)
			// But **not** be annotated with the hash of a different ES spec, meaning
			// it probably still matches the spec of the pre-mutation builder (rolling upgrade not over).
			//
			// Important: this does not catch rolling upgrades due to a keystore change, where the Builder hash
			// would stay the same.
			expectedHash := nodeSetHash(b.Elasticsearch, nodeSet)
			if p.Annotations[BuilderHashAnnotation] != "" && p.Annotations[BuilderHashAnnotation] != expectedHash {
				return fmt.Errorf("pod %s was not upgraded (yet?) to match the expected Elasticsearch specification", p.Name)
			}
		}
	}
	return nil
}

func checkStatefulSetsReplicas(b Builder, k *test.K8sClient) error {
	// build names and replicas count of expected StatefulSets
	expected := make(map[string]int32, len(b.Elasticsearch.Spec.NodeSets)) // map[StatefulSetName]Replicas
	for _, nodeSet := range b.Elasticsearch.Spec.NodeSets {
		expected[estype.StatefulSet(b.Elasticsearch.Name, nodeSet.Name)] = nodeSet.Count
	}
	statefulSets, err := k.GetESStatefulSets(b.Elasticsearch.Namespace, b.Elasticsearch.Name)
	if err != nil {
		return err
	}
	// compare with actual StatefulSets
	actual := make(map[string]int32, len(statefulSets)) // map[StatefulSetName]Replicas
	for _, statefulSet := range statefulSets {
		actual[statefulSet.Name] = *statefulSet.Spec.Replicas // should not be nil
	}
	if !reflect.DeepEqual(expected, actual) {
		return fmt.Errorf("invalid StatefulSets: expected %v, got %v", expected, actual)
	}
	return nil
}

func AnnotatePodsWithBuilderHash(b Builder, k *test.K8sClient) []test.Step {
	return []test.Step{
		{
			Name: "Annotate Pods with a hash of their Builder spec",
			Test: test.Eventually(func() error {
				es := b.Elasticsearch
				for _, nodeSet := range b.Elasticsearch.Spec.NodeSets {
					pods, err := sset.GetActualPodsForStatefulSet(k.Client, types.NamespacedName{
						Namespace: es.Namespace,
						Name:      estype.StatefulSet(es.Name, nodeSet.Name),
					})
					if err != nil {
						return err
					}
					for i := range pods {
						pods[i].Annotations[BuilderHashAnnotation] = nodeSetHash(es, nodeSet)
						if err := k.Client.Update(&pods[i]); err != nil {
							// may error out with a conflict if concurrently updated by the operator,
							// which is why we retry with `test.Eventually`
							return err
						}
					}
				}
				return nil
			}),
		},
		// make sure this is propagated to the local cache so next test steps can expect annotated pods
		{
			Name: "Wait for annotated Pods to appear in the cache",
			Test: test.Eventually(func() error {
				pods, err := sset.GetActualPodsForCluster(k.Client, b.Elasticsearch)
				if err != nil {
					return err
				}
				for _, p := range pods {
					if p.Annotations[BuilderHashAnnotation] == "" {
						return fmt.Errorf("pod %s is not annotated with %s yet", p.Name, BuilderHashAnnotation)
					}
				}
				return nil
			}),
		},
	}
}

// nodeSetHash builds a hash of the nodeSet specification in the given ES resource.
func nodeSetHash(es estype.Elasticsearch, nodeSet estype.NodeSet) string {
	// Normalize the count to zero to exclude it from the hash. Otherwise scaling up/down would affect the hash but
	// existing nodes not affected by the scaling will not be cycled and therefore be annotated with the previous hash.
	nodeSet.Count = 0
	specHash := hash.HashObject(nodeSet)
	esVersionHash := hash.HashObject(es.Spec.Version)
	httpServiceHash := hash.HashObject(es.Spec.HTTP)
	return hash.HashObject(specHash + esVersionHash + httpServiceHash)
}
