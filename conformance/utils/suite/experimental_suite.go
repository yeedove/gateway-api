//go:build experimental
// +build experimental

/*
Copyright 2022 The Kubernetes Authors.

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

package suite

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"

	confv1a1 "sigs.k8s.io/gateway-api/conformance/apis/v1alpha1"
	"sigs.k8s.io/gateway-api/conformance/utils/config"
	"sigs.k8s.io/gateway-api/conformance/utils/kubernetes"
	"sigs.k8s.io/gateway-api/conformance/utils/roundtripper"
)

// -----------------------------------------------------------------------------
// Conformance Test Suite - Public Types
// -----------------------------------------------------------------------------

// ConformanceTestSuite defines the test suite used to run Gateway API
// conformance tests.
type ExperimentalConformanceTestSuite struct {
	ConformanceTestSuite

	// running indicates whether the test suite is currently running
	running bool

	// results stores the pass or fail results of each test that was run by
	// the test suite, organized by the tests unique name.
	results map[string]testResult

	// unsupportedFeatures is a compiled list of named features that were
	// marked as not supported, and is used for reporting the test results.
	unsupportedFeatures sets.Set[SupportedFeature]

	// lock is a mutex to help ensure thread safety of the test suite object.
	lock sync.RWMutex
}

// Options can be used to initialize a ConformanceTestSuite.
type ExperimentalConformanceOptions struct {
	Options

	ConformanceProfiles sets.Set[ConformanceProfileName]
}

// New returns a new ConformanceTestSuite.
func NewExperimentalConformanceTestSuite(s ExperimentalConformanceOptions) (*ExperimentalConformanceTestSuite, error) {
	config.SetupTimeoutConfig(&s.TimeoutConfig)

	roundTripper := s.RoundTripper
	if roundTripper == nil {
		roundTripper = &roundtripper.DefaultRoundTripper{Debug: s.Debug, TimeoutConfig: s.TimeoutConfig}
	}

	suite := &ExperimentalConformanceTestSuite{
		results:             make(map[string]testResult),
		unsupportedFeatures: sets.New[SupportedFeature](),
	}

	// test suite callers are required to provide a conformance profile OR at
	// minimum a list of features which they support.
	if s.SupportedFeatures == nil && s.ConformanceProfiles.Len() == 0 && !s.EnableAllSupportedFeatures {
		return nil, fmt.Errorf("no conformance profile was selected for test run, and no supported features were provided so no tests could be selected")
	}

	// test suite callers can potentially just run all tests by saying they
	// cover all features, if they don't they'll need to have provided a
	// conformance profile or at least some specific features they support.
	if s.EnableAllSupportedFeatures {
		s.SupportedFeatures = AllFeatures
	} else {
		// the use of a conformance profile implicitly enables any features of
		// that profile which are supported at a Core level of support.
		for _, conformanceProfileName := range s.ConformanceProfiles.UnsortedList() {
			conformanceProfile, err := getConformanceProfileForName(conformanceProfileName)
			if err != nil {
				return nil, fmt.Errorf("failed to retrieve conformance profile: %w", err)
			}
			s.SupportedFeatures.Insert(conformanceProfile.CoreFeatures.UnsortedList()...)
		}

		// conformance reports includes a list of features which are NOT supported as well
		// as features that are supported for easy programmatic and human-readable
		// discernment, so we compile that list here for later reporting.
		for _, knownFeature := range AllFeatures.UnsortedList() {
			if !s.SupportedFeatures.Has(knownFeature) {
				suite.unsupportedFeatures.Insert(knownFeature)
			}
		}
	}

	suite.ConformanceTestSuite = ConformanceTestSuite{
		Client:           s.Client,
		RoundTripper:     roundTripper,
		GatewayClassName: s.GatewayClassName,
		Debug:            s.Debug,
		Cleanup:          s.CleanupBaseResources,
		BaseManifests:    s.BaseManifests,
		Applier: kubernetes.Applier{
			NamespaceLabels:          s.NamespaceLabels,
			ValidUniqueListenerPorts: s.ValidUniqueListenerPorts,
		},
		SupportedFeatures: s.SupportedFeatures,
		TimeoutConfig:     s.TimeoutConfig,
		SkipTests:         sets.New(s.SkipTests...),
	}

	// apply defaults
	if suite.BaseManifests == "" {
		suite.BaseManifests = "base/manifests.yaml"
	}

	return suite, nil
}

// -----------------------------------------------------------------------------
// Conformance Test Suite - Public Methods
// -----------------------------------------------------------------------------

// Setup ensures the base resources required for conformance tests are installed
// in the cluster. It also ensures that all relevant resources are ready.
func (suite *ExperimentalConformanceTestSuite) Setup(t *testing.T) {
	t.Logf("Test Setup: Ensuring GatewayClass has been accepted")
	suite.ControllerName = kubernetes.GWCMustHaveAcceptedConditionTrue(t, suite.Client, suite.TimeoutConfig, suite.GatewayClassName)

	suite.Applier.GatewayClass = suite.GatewayClassName
	suite.Applier.ControllerName = suite.ControllerName

	t.Logf("Test Setup: Applying base manifests")
	suite.Applier.MustApplyWithCleanup(t, suite.Client, suite.TimeoutConfig, suite.BaseManifests, suite.Cleanup)

	t.Logf("Test Setup: Applying programmatic resources")
	secret := kubernetes.MustCreateSelfSignedCertSecret(t, "gateway-conformance-web-backend", "certificate", []string{"*"})
	suite.Applier.MustApplyObjectsWithCleanup(t, suite.Client, suite.TimeoutConfig, []client.Object{secret}, suite.Cleanup)
	secret = kubernetes.MustCreateSelfSignedCertSecret(t, "gateway-conformance-infra", "tls-validity-checks-certificate", []string{"*"})
	suite.Applier.MustApplyObjectsWithCleanup(t, suite.Client, suite.TimeoutConfig, []client.Object{secret}, suite.Cleanup)
	secret = kubernetes.MustCreateSelfSignedCertSecret(t, "gateway-conformance-infra", "tls-passthrough-checks-certificate", []string{"abc.example.com"})
	suite.Applier.MustApplyObjectsWithCleanup(t, suite.Client, suite.TimeoutConfig, []client.Object{secret}, suite.Cleanup)

	t.Logf("Test Setup: Ensuring Gateways and Pods from base manifests are ready")
	namespaces := []string{
		"gateway-conformance-infra",
		"gateway-conformance-app-backend",
		"gateway-conformance-web-backend",
	}
	kubernetes.NamespacesMustBeReady(t, suite.Client, suite.TimeoutConfig, namespaces)
}

// Run runs the provided set of conformance tests.
func (suite *ExperimentalConformanceTestSuite) Run(t *testing.T, tests []ConformanceTest) error {
	// verify that the test suite isn't already running, don't start a new run
	// until the previous run finishes
	suite.lock.Lock()
	if suite.running {
		suite.lock.Unlock()
		return fmt.Errorf("can't run the test suite multiple times in parallel: the test suite is already running.")
	}

	// if the test suite is not currently running, reset reporting and start a
	// new test run.
	suite.running = true
	suite.results = nil
	suite.lock.Unlock()

	// run all tests and collect the test results for conformance reporting
	results := make(map[string]testResult)
	for _, test := range tests {
		succeeded := t.Run(test.ShortName, func(t *testing.T) {
			test.Run(t, &suite.ConformanceTestSuite)
		})
		results[test.ShortName] = testResult{
			test:      test,
			succeeded: succeeded,
		}
	}

	// now that the tests have completed, mark the test suite as not running
	// and report the test results.
	suite.lock.Lock()
	suite.running = false
	suite.results = results
	suite.lock.Unlock()

	return nil
}

// Report emits a ConformanceReport for the previously completed test run.
// If no run completed prior to running the report, and error is emitted.
func (suite *ExperimentalConformanceTestSuite) Report() (*confv1a1.ConformanceReport, error) {
	suite.lock.RLock()
	if suite.running {
		suite.lock.RUnlock()
		return nil, fmt.Errorf("can't generate report: the test suite is currently running")
	}
	defer suite.lock.RUnlock()

	profileReports := newReports()
	for _, testResult := range suite.results {
		if err := profileReports.addTestResults(testResult); err != nil {
			return nil, err
		}
	}
	profileReports.compileResults()

	// TODO: need to know which tests were skipped and submit those before
	// the results are compiled.

	// TODO: add handling for supported and unsupported extended features

	return &confv1a1.ConformanceReport{
		Date:              time.Now().Format(time.RFC3339),
		GatewayAPIVersion: "TODO",
		ProfileReports:    profileReports.list(),
	}, nil
}