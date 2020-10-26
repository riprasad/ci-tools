package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	coreapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/fake"
	coreclientset "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"

	"github.com/openshift/ci-tools/pkg/api/secretbootstrap"
	"github.com/openshift/ci-tools/pkg/bitwarden"
)

func TestParseOptions(t *testing.T) {
	testCases := []struct {
		name     string
		given    []string
		expected options
	}{
		{
			name:  "basic case",
			given: []string{"cmd", "--dry-run=false", "--bw-user=username", "--bw-password-path=/tmp/bw-password", "--config=/tmp/config"},
			expected: options{
				bwUser:         "username",
				bwPasswordPath: "/tmp/bw-password",
				configPath:     "/tmp/config",
			},
		},
		{
			name:  "with kubeconfig",
			given: []string{"cmd", "--dry-run=false", "--bw-user=username", "--bw-password-path=/tmp/bw-password", "--config=/tmp/config", "--kubeconfig=/tmp/kubeconfig"},
			expected: options{
				bwUser:         "username",
				bwPasswordPath: "/tmp/bw-password",
				configPath:     "/tmp/config",
				kubeConfigPath: "/tmp/kubeconfig",
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			oldArgs := os.Args
			defer func() { os.Args = oldArgs }()
			os.Args = tc.given
			actual := parseOptions()
			if actual.dryRun != tc.expected.dryRun {
				t.Errorf("%q: (dryRun) actual differs from expected:\n%s", tc.name, cmp.Diff(actual.dryRun, tc.expected.dryRun))
			}
			if actual.bwUser != tc.expected.bwUser {
				t.Errorf("%q: (bwUser) actual differs from expected:\n%s", tc.name, cmp.Diff(actual.bwUser, tc.expected.bwUser))
			}
			if actual.bwPasswordPath != tc.expected.bwPasswordPath {
				t.Errorf("%q: (bwPasswordPath) actual differs from expected:\n%s", tc.name, cmp.Diff(actual.bwPasswordPath, tc.expected.bwPasswordPath))
			}
			if actual.kubeConfigPath != tc.expected.kubeConfigPath {
				t.Errorf("%q: (kubeConfigPath) actual differs from expected:\n%s", tc.name, cmp.Diff(actual.kubeConfigPath, tc.expected.kubeConfigPath))
			}
		})
	}
}

func TestValidateOptions(t *testing.T) {
	testCases := []struct {
		name     string
		given    options
		expected error
	}{
		{
			name: "basic case",
			given: options{
				logLevel:       "info",
				bwUser:         "username",
				bwPasswordPath: "/tmp/bw-password",
				configPath:     "/tmp/config",
			},
		},
		{
			name: "empty bw user",
			given: options{
				logLevel:       "info",
				bwPasswordPath: "/tmp/bw-password",
				configPath:     "/tmp/config",
			},
			expected: fmt.Errorf("--bw-user is empty"),
		},
		{
			name: "empty bw user password path",
			given: options{
				logLevel:   "info",
				bwUser:     "username",
				configPath: "/tmp/config",
			},
			expected: fmt.Errorf("--bw-password-path is empty"),
		},
		{
			name: "empty config path",
			given: options{
				logLevel:       "info",
				bwUser:         "username",
				bwPasswordPath: "/tmp/bw-password",
			},
			expected: fmt.Errorf("--config is empty"),
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actual := tc.given.validateOptions()
			equalError(t, tc.expected, actual)
		})
	}
}

const (
	configContent = `---
secret_configs:
- from:
    key-name-1:
      bw_item: item-name-1
      field: field-name-1
    key-name-2:
      bw_item: item-name-1
      field: field-name-2
    key-name-3:
      bw_item: item-name-1
      attachment: attachment-name-1
    key-name-4:
      bw_item: item-name-2
      field: field-name-1
    key-name-5:
      bw_item: item-name-2
      attachment: attachment-name-1
    key-name-6:
      bw_item: item-name-3
      attachment: attachment-name-2
    key-name-7:
      bw_item: item-name-3
      attribute: password
  to:
    - cluster: default
      namespace: namespace-1
      name: prod-secret-1
    - cluster: build01
      namespace: namespace-2
      name: prod-secret-2
- from:
    .dockerconfigjson:
      bw_item: quay.io
      field: Pull Credentials
  to:
    - cluster: default
      namespace: ci
      name: ci-pull-credentials
      type: kubernetes.io/dockerconfigjson
`
	configContentWithTypo = `---
secret_configs:
- from:
    key-name-1:
      bw_item: item-name-1
      field: field-name-1
    key-name-2:
      bw_item: item-name-1
      field: field-name-2
    key-name-3:
      bw_item: item-name-1
      attachment: attachment-name-1
    key-name-4:
      bw_item: item-name-2
      field: field-name-1
    key-name-5:
      bw_item: item-name-2
      attachment: attachment-name-1
    key-name-6:
      bw_item: item-name-3
      attachment: attachment-name-2
  to:
    - cluster: default
      namespace: namespace-1
      name: prod-secret-1
    - cluster: bla
      namespace: namespace-2
      name: prod-secret-2
`
	configContentWithNonPasswordAttribute = `---
secret_configs:
- from:
    key-name-1:
      bw_item: item-name-1
      field: field-name-1
    key-name-2:
      bw_item: item-name-1
      attribute: not-password
  to:
    - cluster: default
      namespace: namespace-1
      name: prod-secret-1
    - cluster: build01
      namespace: namespace-2
      name: prod-secret-2
`

	configWithGroups = `
cluster_groups:
  group-a:
  - default
secret_configs:
- from:
    key-name-1:
      bw_item: item-name-1
      field: field-name-1
  to:
  - cluster_groups:
    - group-a
    namespace: ns
    name: name
`
	kubeConfigContent = `---
apiVersion: v1
clusters:
- cluster:
    server: https://api.ci.openshift.org:443
  name: api-ci-openshift-org:443
- cluster:
    server: https://api.build01.ci.devcluster.openshift.com:6443
  name: api-build01-ci-devcluster-openshift-com:6443
contexts:
- context:
    cluster: api-build01-ci-devcluster-openshift-com:6443
    namespace: ci
    user: system:serviceaccount:ci:tool/api-build01-ci-devcluster-openshift-com:6443
  name: build01
- context:
    cluster: api-ci-openshift-org:443
    namespace: ci
    user: system:serviceaccount:ci:tool/api-ci-openshift-org:443
  name: default
current-context: default
kind: Config
preferences: {}
users:
- name: system:serviceaccount:ci:tool/api-ci-openshift-org:443
  user:
    token: token1
- name: system:serviceaccount:ci:tool/api-build01-ci-devcluster-openshift-com:6443
  user:
    token: token2
`
)

var (
	configDefault = rest.Config{
		Host:        "https://api.ci.openshift.org:443",
		BearerToken: "token1",
	}
	configBuild01 = rest.Config{
		Host:        "https://api.build01.ci.devcluster.openshift.com:6443",
		BearerToken: "token2",
	}

	defaultConfig = secretbootstrap.Config{
		Secrets: []secretbootstrap.SecretConfig{
			{
				From: map[string]secretbootstrap.BitWardenContext{
					"key-name-1": {
						BWItem: "item-name-1",
						Field:  "field-name-1",
					},
					"key-name-2": {
						BWItem: "item-name-1",
						Field:  "field-name-2",
					},
					"key-name-3": {
						BWItem:     "item-name-1",
						Attachment: "attachment-name-1",
					},
					"key-name-4": {
						BWItem: "item-name-2",
						Field:  "field-name-1",
					},
					"key-name-5": {
						BWItem:     "item-name-2",
						Attachment: "attachment-name-1",
					},
					"key-name-6": {
						BWItem:     "item-name-3",
						Attachment: "attachment-name-2",
					},
					"key-name-7": {
						BWItem:    "item-name-3",
						Attribute: "password",
					},
				},
				To: []secretbootstrap.SecretContext{
					{
						Cluster:   "default",
						Namespace: "namespace-1",
						Name:      "prod-secret-1",
					},
					{
						Cluster:   "build01",
						Namespace: "namespace-2",
						Name:      "prod-secret-2",
					},
				},
			},
			{
				From: map[string]secretbootstrap.BitWardenContext{
					".dockerconfigjson": {
						BWItem: "quay.io",
						Field:  "Pull Credentials",
					},
				},
				To: []secretbootstrap.SecretContext{
					{
						Cluster:   "default",
						Namespace: "ci",
						Name:      "ci-pull-credentials",
						Type:      "kubernetes.io/dockerconfigjson",
					},
				},
			},
		},
	}
	defaultConfigWithoutDefaultCluster = secretbootstrap.Config{
		Secrets: []secretbootstrap.SecretConfig{
			{
				From: map[string]secretbootstrap.BitWardenContext{
					"key-name-1": {
						BWItem: "item-name-1",
						Field:  "field-name-1",
					},
					"key-name-2": {
						BWItem: "item-name-1",
						Field:  "field-name-2",
					},
					"key-name-3": {
						BWItem:     "item-name-1",
						Attachment: "attachment-name-1",
					},
					"key-name-4": {
						BWItem: "item-name-2",
						Field:  "field-name-1",
					},
					"key-name-5": {
						BWItem:     "item-name-2",
						Attachment: "attachment-name-1",
					},
					"key-name-6": {
						BWItem:     "item-name-3",
						Attachment: "attachment-name-2",
					},
					"key-name-7": {
						BWItem:    "item-name-3",
						Attribute: "password",
					},
				},
				To: []secretbootstrap.SecretContext{
					{
						Cluster:   "build01",
						Namespace: "namespace-2",
						Name:      "prod-secret-2",
					},
				},
			},
		},
	}
)

func TestCompleteOptions(t *testing.T) {
	dir, err := ioutil.TempDir("", "test")
	if err != nil {
		t.Errorf("Failed to create temp dir")
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("Failed to remove temp dir")
		}
	}()

	bwPasswordPath := filepath.Join(dir, "bwPasswordPath")
	configPath := filepath.Join(dir, "configPath")
	kubeConfigPath := filepath.Join(dir, "kubeConfigPath")
	configWithTypoPath := filepath.Join(dir, "configWithTypoPath")
	configWithGroupsPath := filepath.Join(dir, "configWithGroups")
	configWithNonPasswordAttributePath := filepath.Join(dir, "configContentWithNonPasswordAttribute")

	fileMap := map[string][]byte{
		bwPasswordPath:                     []byte("topSecret"),
		configPath:                         []byte(configContent),
		kubeConfigPath:                     []byte(kubeConfigContent),
		configWithTypoPath:                 []byte(configContentWithTypo),
		configWithGroupsPath:               []byte(configWithGroups),
		configWithNonPasswordAttributePath: []byte(configContentWithNonPasswordAttribute),
	}

	for k, v := range fileMap {
		if err := ioutil.WriteFile(k, v, 0755); err != nil {
			t.Errorf("Failed to remove temp dir")
		}
	}

	testCases := []struct {
		name               string
		given              options
		expectedError      error
		expectedBWPassword string
		expectedConfig     secretbootstrap.Config
		expectedClusters   []string
	}{
		{
			name: "basic case",
			given: options{
				logLevel:       "info",
				bwUser:         "username",
				bwPasswordPath: bwPasswordPath,
				configPath:     configPath,
				kubeConfigPath: kubeConfigPath,
			},
			expectedBWPassword: "topSecret",
			expectedConfig:     defaultConfig,
			expectedClusters:   []string{"build01", "default"},
		},
		{
			name: "missing context in kubeconfig",
			given: options{
				logLevel:       "info",
				bwUser:         "username",
				bwPasswordPath: bwPasswordPath,
				configPath:     configWithTypoPath,
				kubeConfigPath: kubeConfigPath,
			},
			expectedConfig: defaultConfig,
			expectedError:  fmt.Errorf("config[0].to[1]: failed to find cluster context \"bla\" in the kubeconfig"),
		},
		{
			name: "only configured cluster is used",
			given: options{
				logLevel:       "info",
				bwUser:         "username",
				bwPasswordPath: bwPasswordPath,
				configPath:     configPath,
				kubeConfigPath: kubeConfigPath,
				cluster:        "build01",
			},
			expectedBWPassword: "topSecret",
			expectedConfig:     defaultConfigWithoutDefaultCluster,
			expectedClusters:   []string{"build01"},
		},
		{
			name: "attribute is not password",
			given: options{
				logLevel:       "info",
				bwUser:         "username",
				bwPasswordPath: bwPasswordPath,
				configPath:     configWithNonPasswordAttributePath,
				kubeConfigPath: kubeConfigPath,
			},
			expectedConfig: defaultConfig,
			expectedError:  fmt.Errorf("config[0].from[key-name-2].attribute: only the 'password' is supported, not not-password"),
		},
		{
			name: "group is resolved",
			given: options{
				logLevel:       "info",
				bwUser:         "username",
				bwPasswordPath: bwPasswordPath,
				configPath:     configWithGroupsPath,
				kubeConfigPath: kubeConfigPath,
			},
			expectedBWPassword: "topSecret",
			expectedConfig: secretbootstrap.Config{
				Secrets: []secretbootstrap.SecretConfig{{
					From: map[string]secretbootstrap.BitWardenContext{"key-name-1": {BWItem: "item-name-1", Field: "field-name-1"}},
					To:   []secretbootstrap.SecretContext{{Cluster: "default", Namespace: "ns", Name: "name"}},
				}},
			},
			expectedClusters: []string{"default"},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			secrets := sets.NewString()
			actualError := tc.given.completeOptions(&secrets)
			equalError(t, tc.expectedError, actualError)
			if tc.expectedError == nil {
				equal(t, "bitwarden passowrd", tc.expectedBWPassword, tc.given.bwPassword)
				equal(t, "config", tc.expectedConfig, tc.given.config)
				var actualClusters []string
				for k := range tc.given.secretsGetters {
					actualClusters = append(actualClusters, k)
				}
				sort.Strings(actualClusters)
				equal(t, "clusters", tc.expectedClusters, actualClusters)
				equal(t, "some set", sets.NewString("topSecret"), secrets)
			}
		})
	}
}

func TestValidateCompletedOptions(t *testing.T) {
	testCases := []struct {
		name        string
		given       options
		kubeConfigs map[string]rest.Config
		expected    error
	}{
		{
			name: "basic case",
			given: options{
				logLevel:   "info",
				bwPassword: "topSecret",
				config:     defaultConfig,
			},
			kubeConfigs: map[string]rest.Config{
				"default": configDefault,
				"build01": configBuild01,
			},
		},
		{
			name:     "empty bw password",
			given:    options{bwPasswordPath: "/tmp/password"},
			expected: fmt.Errorf("--bw-password-file was empty"),
		},
		{
			name:     "empty config",
			given:    options{bwPassword: "topSecret"},
			expected: fmt.Errorf("no secrets found to sync"),
		},
		{
			name:     "empty config with cluster filter",
			given:    options{bwPassword: "topSecret", cluster: "cluster"},
			expected: fmt.Errorf("no secrets found to sync for --cluster=cluster"),
		},
		{
			name: "empty to",
			given: options{
				logLevel:   "info",
				bwPassword: "topSecret",
				config: secretbootstrap.Config{
					Secrets: []secretbootstrap.SecretConfig{
						{
							From: map[string]secretbootstrap.BitWardenContext{
								"key-name-1": {
									BWItem: "item-name-1",
									Field:  "field-name-1",
								},
							},
						},
					},
				},
			},
			expected: fmt.Errorf("config[0].to is empty"),
		},
		{
			name: "empty from",
			given: options{
				logLevel:   "info",
				bwPassword: "topSecret",
				config: secretbootstrap.Config{
					Secrets: []secretbootstrap.SecretConfig{{}},
				},
			},
			expected: fmt.Errorf("config[0].from is empty"),
		},
		{
			name: "empty key",
			given: options{
				logLevel:   "info",
				bwPassword: "topSecret",
				config: secretbootstrap.Config{
					Secrets: []secretbootstrap.SecretConfig{
						{
							From: map[string]secretbootstrap.BitWardenContext{
								"": {
									BWItem: "item-name-1",
									Field:  "field-name-1",
								},
							},
							To: []secretbootstrap.SecretContext{
								{
									Cluster:   "default",
									Namespace: "namespace-1",
									Name:      "prod-secret-1",
								},
							},
						},
					},
				},
			},
			expected: fmt.Errorf("config[0].from: empty key is not allowed"),
		},
		{
			name: "empty bw item",
			given: options{
				logLevel:   "info",
				bwPassword: "topSecret",
				config: secretbootstrap.Config{
					Secrets: []secretbootstrap.SecretConfig{
						{
							From: map[string]secretbootstrap.BitWardenContext{
								"key-name-1": {
									Field: "field-name-1",
								},
							},
							To: []secretbootstrap.SecretContext{
								{
									Cluster:   "default",
									Namespace: "namespace-1",
									Name:      "prod-secret-1",
								},
							},
						},
					},
				},
			},
			expected: fmt.Errorf("config[0].from[key-name-1]: empty value is not allowed"),
		},
		{
			name: "empty field and empty attachment",
			given: options{
				logLevel:   "info",
				bwPassword: "topSecret",
				config: secretbootstrap.Config{
					Secrets: []secretbootstrap.SecretConfig{
						{
							From: map[string]secretbootstrap.BitWardenContext{
								"key-name-1": {
									BWItem: "item-name-1",
								},
							},
							To: []secretbootstrap.SecretContext{
								{
									Cluster:   "default",
									Namespace: "namespace-1",
									Name:      "prod-secret-1",
								},
							},
						},
					},
				},
			},
			expected: fmt.Errorf("config[0].from[key-name-1]: one of [field, attachment, attribute] must be set"),
		},
		{
			name: "non-empty field and non-empty attachment",
			given: options{
				logLevel:   "info",
				bwPassword: "topSecret",
				config: secretbootstrap.Config{
					Secrets: []secretbootstrap.SecretConfig{
						{
							From: map[string]secretbootstrap.BitWardenContext{
								"key-name-1": {
									BWItem:     "item-name-1",
									Field:      "field-name-1",
									Attachment: "attachment-name-1",
								},
							},
							To: []secretbootstrap.SecretContext{
								{
									Cluster:   "default",
									Namespace: "namespace-1",
									Name:      "prod-secret-1",
								},
							},
						},
					},
				},
			},
			expected: fmt.Errorf("config[0].from[key-name-1]: cannot use more than one in [field, attachment, attribute]"),
		},
		{
			name: "empty cluster",
			given: options{
				logLevel:   "info",
				bwPassword: "topSecret",
				config: secretbootstrap.Config{
					Secrets: []secretbootstrap.SecretConfig{
						{
							From: map[string]secretbootstrap.BitWardenContext{
								"key-name-1": {
									BWItem: "item-name-1",
									Field:  "field-name-1",
								},
							},
							To: []secretbootstrap.SecretContext{
								{
									Namespace: "namespace-1",
									Name:      "prod-secret-1",
								},
							},
						},
					},
				},
			},
			expected: fmt.Errorf("config[0].to[0].cluster: empty value is not allowed"),
		},
		{
			name: "empty namespace",
			given: options{
				logLevel:   "info",
				bwPassword: "topSecret",
				config: secretbootstrap.Config{
					Secrets: []secretbootstrap.SecretConfig{
						{
							From: map[string]secretbootstrap.BitWardenContext{
								"key-name-1": {
									BWItem:     "item-name-1",
									Attachment: "attachment-name-1",
								},
							},
							To: []secretbootstrap.SecretContext{
								{
									Cluster: "default",
									Name:    "prod-secret-1",
								},
							},
						},
					},
				},
			},
			expected: fmt.Errorf("config[0].to[0].namespace: empty value is not allowed"),
		},
		{
			name: "empty name",
			given: options{
				logLevel:   "info",
				bwPassword: "topSecret",
				config: secretbootstrap.Config{
					Secrets: []secretbootstrap.SecretConfig{
						{
							From: map[string]secretbootstrap.BitWardenContext{
								"key-name-1": {
									BWItem: "item-name-1",
									Field:  "field-name-1",
								},
							},
							To: []secretbootstrap.SecretContext{
								{
									Cluster:   "default",
									Namespace: "namespace-1",
								},
							},
						},
					},
				},
			},
			expected: fmt.Errorf("config[0].to[0].name: empty value is not allowed"),
		},
		{
			name: "conflicting secrets in same TO",
			given: options{
				logLevel:   "info",
				bwPassword: "topSecret",
				config: secretbootstrap.Config{
					Secrets: []secretbootstrap.SecretConfig{
						{
							From: map[string]secretbootstrap.BitWardenContext{
								"key-name-1": {
									BWItem: "item-name-1",
									Field:  "field-name-1",
								},
							},
							To: []secretbootstrap.SecretContext{
								{
									Cluster:   "default",
									Namespace: "namespace-1",
									Name:      "prod-secret-1",
									Type:      "kubernetes.io/dockerconfigjson",
								},
								{
									Cluster:   "build01",
									Namespace: "namespace-1",
									Name:      "prod-secret-1",
								},
								{
									Cluster:   "default",
									Namespace: "namespace-1",
									Name:      "prod-secret-1",
									Type:      "kubernetes.io/dockerconfigjson",
								},
							},
						},
					},
				},
			},
			kubeConfigs: map[string]rest.Config{
				"default": configDefault,
				"build01": configBuild01,
			},
			expected: errors.New("config[0].to[2]: secret namespace-1/prod-secret-1 in cluster default listed more than once in the config"),
		},
		{
			name: "conflicting secrets in different TOs",
			given: options{
				logLevel:   "info",
				bwPassword: "topSecret",
				config: secretbootstrap.Config{
					Secrets: []secretbootstrap.SecretConfig{
						{
							From: map[string]secretbootstrap.BitWardenContext{
								"key-name-1": {
									BWItem: "item-name-1",
									Field:  "field-name-1",
								},
							},
							To: []secretbootstrap.SecretContext{
								{
									Cluster:   "build01",
									Namespace: "namespace-1",
									Name:      "prod-secret-1",
								},
								{
									Cluster:   "default",
									Namespace: "namespace-1",
									Name:      "prod-secret-1",
								},
							},
						},
						{
							From: map[string]secretbootstrap.BitWardenContext{
								"key-name-1": {
									BWItem: "item-name-1",
									Field:  "field-name-1",
								},
							},
							To: []secretbootstrap.SecretContext{
								{
									Cluster:   "default",
									Namespace: "namespace-1",
									Name:      "prod-secret-1",
								},
								{
									Cluster:   "build01",
									Namespace: "namespace-1",
									Name:      "prod-secret-1",
								},
							},
						},
					},
				},
			},
			kubeConfigs: map[string]rest.Config{
				"default": configDefault,
				"build01": configBuild01,
			},
			expected: errors.New("config[1].to[0]: secret namespace-1/prod-secret-1 in cluster default listed more than once in the config"),
		},
		{
			name: "happy dockerconfigJSON configuration",
			given: options{
				logLevel:   "info",
				bwPassword: "topSecret",
				config: secretbootstrap.Config{
					Secrets: []secretbootstrap.SecretConfig{
						{
							From: map[string]secretbootstrap.BitWardenContext{
								"key-name-1": {
									DockerConfigJSONData: []secretbootstrap.DockerConfigJSONData{
										{
											BWItem:                    "bitwarden-item",
											RegistryURLBitwardenField: "registryURL",
											AuthBitwardenAttachment:   "auth",
											EmailBitwardenField:       "email",
										},
										{
											BWItem:                    "bitwarden-item2",
											RegistryURLBitwardenField: "registryURL",
											AuthBitwardenAttachment:   "auth",
											EmailBitwardenField:       "email",
										},
									},
								},
							},
							To: []secretbootstrap.SecretContext{
								{
									Cluster:   "default",
									Name:      "docker-config-json-secret",
									Namespace: "namespace-1",
									Type:      "kubernetes.io/dockerconfigjson",
								},
							},
						},
					},
				},
			},
		},
		{
			name: "happy dockerconfigJSON configuration: use RegistryURL",
			given: options{
				logLevel:   "info",
				bwPassword: "topSecret",
				config: secretbootstrap.Config{
					Secrets: []secretbootstrap.SecretConfig{
						{
							From: map[string]secretbootstrap.BitWardenContext{
								"key-name-1": {
									DockerConfigJSONData: []secretbootstrap.DockerConfigJSONData{
										{
											BWItem:                  "bitwarden-item",
											RegistryURL:             "quay.io",
											AuthBitwardenAttachment: "auth",
											EmailBitwardenField:     "email",
										},
										{
											BWItem:                    "bitwarden-item2",
											RegistryURLBitwardenField: "registryURL",
											AuthBitwardenAttachment:   "auth",
											EmailBitwardenField:       "email",
										},
									},
								},
							},
							To: []secretbootstrap.SecretContext{
								{
									Cluster:   "default",
									Name:      "docker-config-json-secret",
									Namespace: "namespace-1",
									Type:      "kubernetes.io/dockerconfigjson",
								},
							},
						},
					},
				},
			},
		},
		{
			name: "sad dockerconfigJSON configuration: cannot set both RegistryURL and RegistryURLBitwardenField",
			given: options{
				logLevel:   "info",
				bwPassword: "topSecret",
				config: secretbootstrap.Config{
					Secrets: []secretbootstrap.SecretConfig{
						{
							From: map[string]secretbootstrap.BitWardenContext{
								"key-name-1": {
									DockerConfigJSONData: []secretbootstrap.DockerConfigJSONData{
										{
											BWItem:                    "bitwarden-item",
											RegistryURL:               "quay.io",
											RegistryURLBitwardenField: "registryURL",
											AuthBitwardenAttachment:   "auth",
											EmailBitwardenField:       "email",
										},
									},
								},
							},
							To: []secretbootstrap.SecretContext{
								{
									Cluster:   "default",
									Name:      "docker-config-json-secret",
									Namespace: "namespace-1",
									Type:      "kubernetes.io/dockerconfigjson",
								},
							},
						},
					},
				},
			},
			expected: fmt.Errorf("config[0].from[key-name-1]: registry_url_bw_field and registry_url are mutualy exclusive"),
		},
		{
			name: "sad dockerconfigJSON configuration",
			given: options{
				logLevel:   "info",
				bwPassword: "topSecret",
				config: secretbootstrap.Config{
					Secrets: []secretbootstrap.SecretConfig{
						{
							From: map[string]secretbootstrap.BitWardenContext{
								"key-name-1": {
									DockerConfigJSONData: []secretbootstrap.DockerConfigJSONData{
										{
											BWItem:                    "bitwarden-item",
											RegistryURLBitwardenField: "registryURL",
										},
										{
											BWItem:                    "bitwarden-item2",
											RegistryURLBitwardenField: "registryURL",
											AuthBitwardenAttachment:   "auth",
											EmailBitwardenField:       "email",
										},
									},
								},
							},
							To: []secretbootstrap.SecretContext{
								{
									Cluster:   "default",
									Name:      "docker-config-json-secret",
									Namespace: "namespace-1",
								},
							},
						},
					},
				},
			},
			expected: fmt.Errorf("config[0].from[key-name-1]: auth_bw_attachment is missing"),
		},
		{
			name: "sad dockerconfigJSON configuration: cannot determine registry URL",
			given: options{
				logLevel:   "info",
				bwPassword: "topSecret",
				config: secretbootstrap.Config{
					Secrets: []secretbootstrap.SecretConfig{
						{
							From: map[string]secretbootstrap.BitWardenContext{
								"key-name-1": {
									DockerConfigJSONData: []secretbootstrap.DockerConfigJSONData{
										{
											BWItem:                  "bitwarden-item2",
											AuthBitwardenAttachment: "auth",
											EmailBitwardenField:     "email",
										},
									},
								},
							},
							To: []secretbootstrap.SecretContext{
								{
									Cluster:   "default",
									Name:      "docker-config-json-secret",
									Namespace: "namespace-1",
								},
							},
						},
					},
				},
			},
			expected: fmt.Errorf("config[0].from[key-name-1]: either registry_url_bw_field or registry_url must be set"),
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actual := tc.given.validateCompletedOptions()
			equalError(t, tc.expected, actual)
		})
	}
}

func TestConstructSecrets(t *testing.T) {
	testCases := []struct {
		name          string
		config        secretbootstrap.Config
		bwClient      bitwarden.Client
		expected      map[string][]*coreapi.Secret
		expectedError error
	}{
		{
			name:   "basic case",
			config: defaultConfig,
			bwClient: bitwarden.NewFakeClient(
				[]bitwarden.Item{
					{
						ID:   "1",
						Name: "item-name-1",
						Fields: []bitwarden.Field{
							{
								Name:  "field-name-1",
								Value: "value1",
							},
							{
								Name:  "field-name-2",
								Value: "value2",
							},
						},
						Attachments: []bitwarden.Attachment{
							{
								ID:       "a-id-1-1",
								FileName: "attachment-name-1",
							},
							{
								ID:       "a-id-1-2",
								FileName: "attachment-name-2",
							},
						},
					},
					{
						ID:   "2",
						Name: "item-name-2",
						Fields: []bitwarden.Field{
							{
								Name:  "field-name-1",
								Value: "value3",
							},
							{
								Name:  "field-name-2",
								Value: "value2",
							},
						},
						Attachments: []bitwarden.Attachment{
							{
								ID:       "a-id-2-1",
								FileName: "attachment-name-1",
							},
							{
								ID:       "a-id-2-2",
								FileName: "attachment-name-2",
							},
						},
					},
					{
						ID:   "3",
						Name: "item-name-3",
						Login: &bitwarden.Login{
							Password: "yyy",
						},
						Fields: []bitwarden.Field{
							{
								Name:  "field-name-1",
								Value: "value1",
							},
						},
						Attachments: []bitwarden.Attachment{
							{
								ID:       "a-id-3-1",
								FileName: "attachment-name-1",
							},
							{
								ID:       "a-id-3-2",
								FileName: "attachment-name-2",
							},
						},
					},
					{
						ID:   "a",
						Name: "quay.io",
						Fields: []bitwarden.Field{
							{
								Name:  "Pull Credentials",
								Value: "123",
							},
						},
					},
				},
				map[string]string{
					"a-id-1-1": "attachment-name-1-1-value",
					"a-id-2-1": "attachment-name-2-1-value",
					"a-id-3-2": "attachment-name-3-2-value",
				},
			),
			expected: map[string][]*coreapi.Secret{
				"default": {
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "prod-secret-1",
							Namespace: "namespace-1",
							Labels:    map[string]string{"ci.openshift.org/auto-managed": "true"},
						},
						Data: map[string][]byte{
							"key-name-1": []byte("value1"),
							"key-name-2": []byte("value2"),
							"key-name-3": []byte("attachment-name-1-1-value"),
							"key-name-4": []byte("value3"),
							"key-name-5": []byte("attachment-name-2-1-value"),
							"key-name-6": []byte("attachment-name-3-2-value"),
							"key-name-7": []byte("yyy"),
						},
						Type: "Opaque",
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "ci-pull-credentials",
							Namespace: "ci",
							Labels:    map[string]string{"ci.openshift.org/auto-managed": "true"},
						},
						Data: map[string][]byte{
							".dockerconfigjson": []byte("123"),
						},
						Type: "kubernetes.io/dockerconfigjson",
					},
				},
				"build01": {
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "prod-secret-2",
							Namespace: "namespace-2",
							Labels:    map[string]string{"ci.openshift.org/auto-managed": "true"},
						},
						Data: map[string][]byte{
							"key-name-1": []byte("value1"),
							"key-name-2": []byte("value2"),
							"key-name-3": []byte("attachment-name-1-1-value"),
							"key-name-4": []byte("value3"),
							"key-name-5": []byte("attachment-name-2-1-value"),
							"key-name-6": []byte("attachment-name-3-2-value"),
							"key-name-7": []byte("yyy"),
						},
						Type: "Opaque",
					},
				},
			},
		},
		{
			name:   "error: no such a field",
			config: defaultConfig,
			bwClient: bitwarden.NewFakeClient(
				[]bitwarden.Item{
					{
						ID:   "1",
						Name: "item-name-1",
						Fields: []bitwarden.Field{
							{
								Name:  "field-name-2",
								Value: "value2",
							},
						},
						Attachments: []bitwarden.Attachment{
							{
								ID:       "a-id-1-1",
								FileName: "attachment-name-1",
							},
							{
								ID:       "a-id-1-2",
								FileName: "attachment-name-2",
							},
						},
					},
					{
						ID:   "2",
						Name: "item-name-2",
						Fields: []bitwarden.Field{
							{
								Name:  "field-name-1",
								Value: "value3",
							},
							{
								Name:  "field-name-2",
								Value: "value2",
							},
						},
						Attachments: []bitwarden.Attachment{
							{
								ID:       "a-id-2-1",
								FileName: "attachment-name-1",
							},
							{
								ID:       "a-id-2-2",
								FileName: "attachment-name-2",
							},
						},
					},
					{
						ID:   "3",
						Name: "item-name-3",
						Login: &bitwarden.Login{
							Password: "yyy",
						},
						Fields: []bitwarden.Field{
							{
								Name:  "field-name-1",
								Value: "value1",
							},
						},
						Attachments: []bitwarden.Attachment{
							{
								ID:       "a-id-3-1",
								FileName: "attachment-name-1",
							},
							{
								ID:       "a-id-3-2",
								FileName: "attachment-name-2",
							},
						},
					},
				},
				map[string]string{
					"a-id-1-1": "attachment-name-1-1-value",
					"a-id-2-1": "attachment-name-2-1-value",
					"a-id-3-2": "attachment-name-3-2-value",
				},
			),
			expectedError: fmt.Errorf("[failed to find field Pull Credentials in item quay.io, failed to find field field-name-1 in item item-name-1]"),
		},
		{
			name:   "error: no such an attachment",
			config: defaultConfig,
			bwClient: bitwarden.NewFakeClient(
				[]bitwarden.Item{
					{
						ID:    "1",
						Name:  "item-name-1",
						Login: &bitwarden.Login{Password: "abc"},
						Fields: []bitwarden.Field{
							{
								Name:  "field-name-1",
								Value: "value1",
							},
							{
								Name:  "field-name-2",
								Value: "value2",
							},
						},
						Attachments: []bitwarden.Attachment{
							{
								ID:       "a-id-1-1",
								FileName: "attachment-name-1",
							},
							{
								ID:       "a-id-1-2",
								FileName: "attachment-name-2",
							},
						},
					},
					{
						ID:   "2",
						Name: "item-name-2",
						Fields: []bitwarden.Field{
							{
								Name:  "field-name-1",
								Value: "value3",
							},
							{
								Name:  "field-name-2",
								Value: "value2",
							},
						},
						Attachments: []bitwarden.Attachment{
							{
								ID:       "a-id-2-2",
								FileName: "attachment-name-2",
							},
						},
					},
					{
						ID:   "3",
						Name: "item-name-3",
						Fields: []bitwarden.Field{
							{
								Name:  "field-name-1",
								Value: "value1",
							},
						},
						Attachments: []bitwarden.Attachment{
							{
								ID:       "a-id-3-1",
								FileName: "attachment-name-1",
							},
							{
								ID:       "a-id-3-2",
								FileName: "attachment-name-2",
							},
						},
					},
				},
				map[string]string{
					"a-id-1-1": "attachment-name-1-1-value",
					"a-id-3-2": "attachment-name-3-2-value",
				},
			),
			expectedError: fmt.Errorf("[failed to find attachment attachment-name-1 in item item-name-2, failed to find field Pull Credentials in item quay.io, failed to find password in item item-name-3]"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actual, actualError := constructSecrets(context.TODO(), tc.config, tc.bwClient, 10)
			equalError(t, tc.expectedError, actualError)
			if actualError != nil {
				return
			}
			for key := range actual {
				sort.Slice(actual[key], func(i, j int) bool {
					return actual[key][i].Name < actual[key][j].Name
				})
			}
			for key := range tc.expected {
				sort.Slice(tc.expected[key], func(i, j int) bool {
					return tc.expected[key][i].Name < tc.expected[key][j].Name
				})
			}
			equal(t, "secrets", tc.expected, actual)
		})
	}
}

func TestUpdateSecrets(t *testing.T) {
	testCases := []struct {
		name                     string
		existSecretsOnDefault    []runtime.Object
		existSecretsOnBuild01    []runtime.Object
		secretsMap               map[string][]*coreapi.Secret
		force                    bool
		expected                 error
		expectedSecretsOnDefault []coreapi.Secret
		expectedSecretsOnBuild01 []coreapi.Secret
	}{
		{
			name: "basic case with force",
			existSecretsOnDefault: []runtime.Object{
				&coreapi.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "prod-secret-1",
						Namespace: "namespace-1",
					},
					Data: map[string][]byte{
						"key-name-1": []byte("abc"),
					},
				},
			},
			secretsMap: map[string][]*coreapi.Secret{
				"default": {
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "prod-secret-1",
							Namespace: "namespace-1",
							Labels:    map[string]string{"ci.openshift.org/auto-managed": "true"},
						},
						Data: map[string][]byte{
							"key-name-1": []byte("value1"),
							"key-name-2": []byte("value2"),
							"key-name-3": []byte("attachment-name-1-1-value"),
							"key-name-4": []byte("value3"),
							"key-name-5": []byte("attachment-name-2-1-value"),
							"key-name-6": []byte("attachment-name-3-2-value"),
							"key-name-7": []byte("yyy"),
						},
					},
				},
				"build01": {
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "prod-secret-2",
							Namespace: "namespace-2",
							Labels:    map[string]string{"ci.openshift.org/auto-managed": "true"},
						},
						Data: map[string][]byte{
							"key-name-1": []byte("value1"),
							"key-name-2": []byte("value2"),
							"key-name-3": []byte("attachment-name-1-1-value"),
							"key-name-4": []byte("value3"),
							"key-name-5": []byte("attachment-name-2-1-value"),
							"key-name-6": []byte("attachment-name-3-2-value"),
							"key-name-7": []byte("yyy"),
						},
					},
				},
			},
			force: true,
			expectedSecretsOnDefault: []coreapi.Secret{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "prod-secret-1",
						Namespace: "namespace-1",
						Labels:    map[string]string{"ci.openshift.org/auto-managed": "true"},
					},
					Data: map[string][]byte{
						"key-name-1": []byte("value1"),
						"key-name-2": []byte("value2"),
						"key-name-3": []byte("attachment-name-1-1-value"),
						"key-name-4": []byte("value3"),
						"key-name-5": []byte("attachment-name-2-1-value"),
						"key-name-6": []byte("attachment-name-3-2-value"),
						"key-name-7": []byte("yyy"),
					},
				},
			},
			expectedSecretsOnBuild01: []coreapi.Secret{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "prod-secret-2",
						Namespace: "namespace-2",
						Labels:    map[string]string{"ci.openshift.org/auto-managed": "true"},
					},
					Data: map[string][]byte{
						"key-name-1": []byte("value1"),
						"key-name-2": []byte("value2"),
						"key-name-3": []byte("attachment-name-1-1-value"),
						"key-name-4": []byte("value3"),
						"key-name-5": []byte("attachment-name-2-1-value"),
						"key-name-6": []byte("attachment-name-3-2-value"),
						"key-name-7": []byte("yyy"),
					},
				},
			},
		},
		{
			name: "basic case with force, unrelated keys are kept",
			existSecretsOnDefault: []runtime.Object{
				&coreapi.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "prod-secret-1",
						Namespace: "namespace-1",
					},
					Data: map[string][]byte{
						"key-name-1": []byte("abc"),
						"unmanaged":  []byte("data"),
					},
				},
			},
			secretsMap: map[string][]*coreapi.Secret{
				"default": {
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "prod-secret-1",
							Namespace: "namespace-1",
							Labels:    map[string]string{"ci.openshift.org/auto-managed": "true"},
						},
						Data: map[string][]byte{
							"key-name-1": []byte("value1"),
							"key-name-2": []byte("value2"),
							"key-name-3": []byte("attachment-name-1-1-value"),
							"key-name-4": []byte("value3"),
							"key-name-5": []byte("attachment-name-2-1-value"),
							"key-name-6": []byte("attachment-name-3-2-value"),
							"key-name-7": []byte("yyy"),
						},
					},
				},
				"build01": {
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "prod-secret-2",
							Namespace: "namespace-2",
							Labels:    map[string]string{"ci.openshift.org/auto-managed": "true"},
						},
						Data: map[string][]byte{
							"key-name-1": []byte("value1"),
							"key-name-2": []byte("value2"),
							"key-name-3": []byte("attachment-name-1-1-value"),
							"key-name-4": []byte("value3"),
							"key-name-5": []byte("attachment-name-2-1-value"),
							"key-name-6": []byte("attachment-name-3-2-value"),
							"key-name-7": []byte("yyy"),
						},
					},
				},
			},
			force: true,
			expectedSecretsOnDefault: []coreapi.Secret{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "prod-secret-1",
						Namespace: "namespace-1",
						Labels:    map[string]string{"ci.openshift.org/auto-managed": "true"},
					},
					Data: map[string][]byte{
						"key-name-1": []byte("value1"),
						"key-name-2": []byte("value2"),
						"key-name-3": []byte("attachment-name-1-1-value"),
						"key-name-4": []byte("value3"),
						"key-name-5": []byte("attachment-name-2-1-value"),
						"key-name-6": []byte("attachment-name-3-2-value"),
						"key-name-7": []byte("yyy"),
						"unmanaged":  []byte("data"),
					},
				},
			},
			expectedSecretsOnBuild01: []coreapi.Secret{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "prod-secret-2",
						Namespace: "namespace-2",
						Labels:    map[string]string{"ci.openshift.org/auto-managed": "true"},
					},
					Data: map[string][]byte{
						"key-name-1": []byte("value1"),
						"key-name-2": []byte("value2"),
						"key-name-3": []byte("attachment-name-1-1-value"),
						"key-name-4": []byte("value3"),
						"key-name-5": []byte("attachment-name-2-1-value"),
						"key-name-6": []byte("attachment-name-3-2-value"),
						"key-name-7": []byte("yyy"),
					},
				},
			},
		},
		{
			name: "basic case without force: not semantically equal",
			existSecretsOnDefault: []runtime.Object{
				&coreapi.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "prod-secret-1",
						Namespace: "namespace-1",
						Labels:    map[string]string{"ci.openshift.org/auto-managed": "true"},
					},
					Data: map[string][]byte{
						"key-name-1": []byte("abc"),
					},
				},
			},
			secretsMap: map[string][]*coreapi.Secret{
				"default": {
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "prod-secret-1",
							Namespace: "namespace-1",
							Labels:    map[string]string{"ci.openshift.org/auto-managed": "true"},
						},
						Data: map[string][]byte{
							"key-name-1": []byte("value1"),
						},
					},
				},
			},
			expected: fmt.Errorf("secret default:namespace-1/prod-secret-1 needs updating in place, use --force to do so"),
			expectedSecretsOnDefault: []coreapi.Secret{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "prod-secret-1",
						Namespace: "namespace-1",
						Labels:    map[string]string{"ci.openshift.org/auto-managed": "true"},
					},
					Data: map[string][]byte{
						"key-name-1": []byte("abc"),
					},
				},
			},
		},
		{
			name: "basic case without force: semantically equal",
			existSecretsOnDefault: []runtime.Object{
				&coreapi.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "prod-secret-1",
						Namespace:         "namespace-1",
						Labels:            map[string]string{"ci.openshift.org/auto-managed": "true"},
						CreationTimestamp: metav1.NewTime(time.Now()),
					},
					Data: map[string][]byte{
						"key-name-1": []byte("abc"),
					},
				},
			},
			secretsMap: map[string][]*coreapi.Secret{
				"default": {
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "prod-secret-1",
							Namespace: "namespace-1",
							Labels:    map[string]string{"ci.openshift.org/auto-managed": "true"},
						},
						Data: map[string][]byte{
							"key-name-1": []byte("abc"),
						},
					},
				},
			},
			expectedSecretsOnDefault: []coreapi.Secret{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "prod-secret-1",
						Namespace: "namespace-1",
						Labels:    map[string]string{"ci.openshift.org/auto-managed": "true"},
					},
					Data: map[string][]byte{
						"key-name-1": []byte("abc"),
					},
				},
			},
		},
		{
			name: "change secret type with force",
			existSecretsOnDefault: []runtime.Object{
				&coreapi.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "prod-secret-2",
						Namespace: "namespace-2",
					},
					Data: map[string][]byte{
						"key-name-1": []byte(`{
  "auths": {
    "quay.io": {
      "auth": "aaa",
      "email": ""
    }
  }
}`),
					},
					Type: coreapi.SecretTypeDockerConfigJson,
				},
			},
			secretsMap: map[string][]*coreapi.Secret{
				"default": {
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "prod-secret-2",
							Namespace: "namespace-2",
						},
						Data: map[string][]byte{
							"key-name-1": []byte(`{
  "auths": {
    "quay.io": {
      "auth": "aaa",
      "email": ""
    }
  }
}`),
						},
					},
				},
			},
			force:    true,
			expected: fmt.Errorf("cannot change secret type from \"kubernetes.io/dockerconfigjson\" to \"\" (immutable field): default:namespace-2/prod-secret-2"),
			expectedSecretsOnDefault: []coreapi.Secret{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "prod-secret-2",
						Namespace: "namespace-2",
					},
					Data: map[string][]byte{
						"key-name-1": []byte(`{
  "auths": {
    "quay.io": {
      "auth": "aaa",
      "email": ""
    }
  }
}`),
					},
					Type: coreapi.SecretTypeDockerConfigJson,
				},
			},
		},
		{
			name: "change secret type without force",
			existSecretsOnDefault: []runtime.Object{
				&coreapi.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "prod-secret-2",
						Namespace: "namespace-2",
					},
					Data: map[string][]byte{
						"key-name-1": []byte(`{
  "auths": {
    "quay.io": {
      "auth": "aaa",
      "email": ""
    }
  }
}`),
					},
					Type: coreapi.SecretTypeDockerConfigJson,
				},
			},
			secretsMap: map[string][]*coreapi.Secret{
				"default": {
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "prod-secret-2",
							Namespace: "namespace-2",
						},
						Data: map[string][]byte{
							"key-name-1": []byte(`{
  "auths": {
    "quay.io": {
      "auth": "aaa",
      "email": ""
    }
  }
}`),
						},
					},
				},
			},
			expected: fmt.Errorf("cannot change secret type from \"kubernetes.io/dockerconfigjson\" to \"\" (immutable field): default:namespace-2/prod-secret-2"),
			expectedSecretsOnDefault: []coreapi.Secret{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "prod-secret-2",
						Namespace: "namespace-2",
					},
					Data: map[string][]byte{
						"key-name-1": []byte(`{
  "auths": {
    "quay.io": {
      "auth": "aaa",
      "email": ""
    }
  }
}`),
					},
					Type: coreapi.SecretTypeDockerConfigJson,
				},
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fkcDefault := fake.NewSimpleClientset(tc.existSecretsOnDefault...)
			fkcBuild01 := fake.NewSimpleClientset(tc.existSecretsOnBuild01...)
			clients := map[string]coreclientset.SecretsGetter{
				"default": fkcDefault.CoreV1(),
				"build01": fkcBuild01.CoreV1(),
			}

			actual := updateSecrets(clients, tc.secretsMap, tc.force)
			equalError(t, tc.expected, actual)

			actualSecretsOnDefault, err := fkcDefault.CoreV1().Secrets("").List(context.TODO(), metav1.ListOptions{})
			equalError(t, nil, err)
			equal(t, "secrets in default cluster", tc.expectedSecretsOnDefault, actualSecretsOnDefault.Items)

			actualSecretsOnBuild01, err := fkcBuild01.CoreV1().Secrets("").List(context.TODO(), metav1.ListOptions{})
			equalError(t, nil, err)
			equal(t, "secrets in build01 cluster", tc.expectedSecretsOnBuild01, actualSecretsOnBuild01.Items)
		})
	}
}

func TestWriteSecrets(t *testing.T) {
	testCases := []struct {
		name          string
		secretsMap    map[string][]*coreapi.Secret
		w             *bytes.Buffer
		expected      string
		expectedError error
	}{
		{
			name: "basic case",
			secretsMap: map[string][]*coreapi.Secret{
				"default": {
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "prod-secret-1",
							Namespace: "namespace-1",
							Labels:    map[string]string{"ci.openshift.org/auto-managed": "true"},
						},
						Data: map[string][]byte{
							"key-name-1": []byte("value1"),
							"key-name-2": []byte("value2"),
							"key-name-3": []byte("attachment-name-1-1-value"),
							"key-name-4": []byte("value3"),
							"key-name-5": []byte("attachment-name-2-1-value"),
							"key-name-6": []byte("attachment-name-3-2-value"),
						},
					},
				},
				"build01": {
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "prod-secret-2",
							Namespace: "namespace-2",
							Labels:    map[string]string{"ci.openshift.org/auto-managed": "true"},
						},
						Data: map[string][]byte{
							"key-name-1": []byte("value1"),
							"key-name-2": []byte("value2"),
							"key-name-3": []byte("attachment-name-1-1-value"),
							"key-name-4": []byte("value3"),
							"key-name-5": []byte("attachment-name-2-1-value"),
							"key-name-6": []byte("attachment-name-3-2-value"),
						},
					},
				},
			},
			w: &bytes.Buffer{},
			expected: `###build01###
---
data:
  key-name-1: dmFsdWUx
  key-name-2: dmFsdWUy
  key-name-3: YXR0YWNobWVudC1uYW1lLTEtMS12YWx1ZQ==
  key-name-4: dmFsdWUz
  key-name-5: YXR0YWNobWVudC1uYW1lLTItMS12YWx1ZQ==
  key-name-6: YXR0YWNobWVudC1uYW1lLTMtMi12YWx1ZQ==
metadata:
  creationTimestamp: null
  labels:
    ci.openshift.org/auto-managed: "true"
  name: prod-secret-2
  namespace: namespace-2
###default###
---
data:
  key-name-1: dmFsdWUx
  key-name-2: dmFsdWUy
  key-name-3: YXR0YWNobWVudC1uYW1lLTEtMS12YWx1ZQ==
  key-name-4: dmFsdWUz
  key-name-5: YXR0YWNobWVudC1uYW1lLTItMS12YWx1ZQ==
  key-name-6: YXR0YWNobWVudC1uYW1lLTMtMi12YWx1ZQ==
metadata:
  creationTimestamp: null
  labels:
    ci.openshift.org/auto-managed: "true"
  name: prod-secret-1
  namespace: namespace-1
`,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actualError := writeSecrets(tc.secretsMap, tc.w)
			equalError(t, tc.expectedError, actualError)
			equal(t, "result", tc.expected, tc.w.String())
		})
	}
}

func equalError(t *testing.T, expected, actual error) {
	if expected != nil && actual == nil || expected == nil && actual != nil {
		t.Errorf("expecting error \"%v\", got \"%v\"", expected, actual)
	}
	if expected != nil && actual != nil && expected.Error() != actual.Error() {
		t.Errorf("expecting error msg %q, got %q", expected.Error(), actual.Error())
	}
}

func equal(t *testing.T, what string, expected, actual interface{}) {
	if !reflect.DeepEqual(expected, actual) {
		t.Errorf("%s differs from expected:\n%s", what, cmp.Diff(expected, actual))
	}
}

func TestConstructDockerConfigJSON(t *testing.T) {
	type attachment struct {
		bwItem   string
		filename string
		contents []byte
	}
	testCases := []struct {
		id                   string
		bwClient             bitwarden.Client
		dockerConfigJSONData []secretbootstrap.DockerConfigJSONData
		attachments          []attachment
		expectedJSON         []byte
		expectedError        string
	}{
		{
			id: "happy case",
			attachments: []attachment{
				{
					bwItem:   "item-name-1",
					filename: "auth",
					contents: []byte("123456789"),
				},
			},
			dockerConfigJSONData: []secretbootstrap.DockerConfigJSONData{
				{
					BWItem:                    "item-name-1",
					RegistryURLBitwardenField: "registryURL",
					AuthBitwardenAttachment:   "auth",
					EmailBitwardenField:       "email",
				},
			},
			bwClient: bitwarden.NewFakeClient(
				[]bitwarden.Item{
					{
						ID:   "1",
						Name: "item-name-1",
						Attachments: []bitwarden.Attachment{
							{
								ID:       "12345678",
								FileName: "auth",
							},
						},
						Fields: []bitwarden.Field{
							{
								Name:  "registryURL",
								Value: "quay.io",
							},
							{
								Name:  "email",
								Value: "test@test.com",
							},
						},
					},
				}, make(map[string]string)),
			expectedJSON: []byte(`{"auths":{"quay.io":{"auth":"123456789","email":"test@test.com"}}}`),
		},
		{
			id: "RegistryURL overrides RegistryURLBitwardenField",
			attachments: []attachment{
				{
					bwItem:   "item-name-1",
					filename: "auth",
					contents: []byte("123456789"),
				},
			},
			dockerConfigJSONData: []secretbootstrap.DockerConfigJSONData{
				{
					BWItem:                    "item-name-1",
					RegistryURLBitwardenField: "registryURL",
					AuthBitwardenAttachment:   "auth",
					EmailBitwardenField:       "email",
					RegistryURL:               "cool-url",
				},
			},
			bwClient: bitwarden.NewFakeClient(
				[]bitwarden.Item{
					{
						ID:   "1",
						Name: "item-name-1",
						Attachments: []bitwarden.Attachment{
							{
								ID:       "12345678",
								FileName: "auth",
							},
						},
						Fields: []bitwarden.Field{
							{
								Name:  "registryURL",
								Value: "quay.io",
							},
							{
								Name:  "email",
								Value: "test@test.com",
							},
						},
					},
				}, make(map[string]string)),
			expectedJSON: []byte(`{"auths":{"cool-url":{"auth":"123456789","email":"test@test.com"}}}`),
		},
		{
			id: "happy multiple case",
			attachments: []attachment{
				{
					bwItem:   "item-name-1",
					filename: "auth",
					contents: []byte("123456789"),
				},
				{
					bwItem:   "item-name-2",
					filename: "auth",
					contents: []byte("987654321"),
				},
			},
			dockerConfigJSONData: []secretbootstrap.DockerConfigJSONData{
				{
					BWItem:                    "item-name-1",
					RegistryURLBitwardenField: "registryURL",
					AuthBitwardenAttachment:   "auth",
					EmailBitwardenField:       "email",
				},
				{
					BWItem:                    "item-name-2",
					RegistryURLBitwardenField: "registryURL",
					AuthBitwardenAttachment:   "auth",
					EmailBitwardenField:       "email",
				},
			},
			bwClient: bitwarden.NewFakeClient(
				[]bitwarden.Item{
					{
						ID:   "1",
						Name: "item-name-1",
						Fields: []bitwarden.Field{
							{
								Name:  "registryURL",
								Value: "quay.io",
							},
							{
								Name:  "auth",
								Value: "123456789",
							},
							{
								Name:  "email",
								Value: "test@test.com",
							},
						},
					},
					{
						ID:   "2",
						Name: "item-name-2",
						Fields: []bitwarden.Field{
							{
								Name:  "registryURL",
								Value: "cloud.redhat.com",
							},
							{
								Name:  "auth",
								Value: "987654321",
							},
							{
								Name:  "email",
								Value: "foo@bar.com",
							},
						},
					},
				}, make(map[string]string)),
			expectedJSON: []byte(`{"auths":{"cloud.redhat.com":{"auth":"987654321","email":"foo@bar.com"},"quay.io":{"auth":"123456789","email":"test@test.com"}}}`),
		},
		{
			id: "sad case, field is missing",
			dockerConfigJSONData: []secretbootstrap.DockerConfigJSONData{
				{
					BWItem:                    "item-name-1",
					RegistryURLBitwardenField: "registryURL",
					AuthBitwardenAttachment:   "auth",
					EmailBitwardenField:       "email",
				},
			},
			bwClient: bitwarden.NewFakeClient(
				[]bitwarden.Item{
					{
						ID:   "1",
						Name: "item-name-1",
						Fields: []bitwarden.Field{
							{
								Name:  "registryURL",
								Value: "quay.io",
							},
							{
								Name:  "email",
								Value: "test@test.com",
							},
						},
					},
				}, nil),
			expectedError: "couldn't get attachment 'auth' from bw item item-name-1: failed to find attachment auth in item item-name-1",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.id, func(t *testing.T) {
			if len(tc.attachments) > 0 {
				for _, attachment := range tc.attachments {
					if err := tc.bwClient.SetAttachmentOnItem(attachment.bwItem, attachment.filename, attachment.contents); err != nil {
						t.Fatalf("couldn't create attachments: %v", err)
					}
				}
			}
			actual, err := constructDockerConfigJSON(tc.bwClient, tc.dockerConfigJSONData)
			if tc.expectedError != "" && err != nil {
				if !reflect.DeepEqual(err.Error(), tc.expectedError) {
					t.Fatal(cmp.Diff(err.Error(), tc.expectedError))
				}
			} else if tc.expectedError == "" && err != nil {
				t.Fatalf("Error not expected: %v", err)
			} else {
				if !reflect.DeepEqual(actual, tc.expectedJSON) {
					t.Fatal(cmp.Diff(actual, tc.expectedJSON))
				}
			}
		})
	}
}
