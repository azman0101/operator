/*
Copyright AppsCode Inc. and Contributors

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

package validator

import (
	"context"
	"fmt"
	"strings"

	api "kubedb.dev/apimachinery/apis/kubedb/v1alpha2"

	"github.com/pkg/errors"
	"gomodules.xyz/x/arrays"
	core "k8s.io/api/core/v1"
	kerr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	mona "kmodules.xyz/monitoring-agent-api/api/v1"
)

func ValidateStorage(client kubernetes.Interface, storageType api.StorageType, spec *core.PersistentVolumeClaimSpec, storageSpecPath ...string) error {
	if storageType == api.StorageTypeEphemeral {
		return nil
	}

	storagePath := "spec.storage"
	if len(storageSpecPath) != 0 {
		storagePath = strings.Join(storageSpecPath, ".")
	}

	if spec == nil {
		return fmt.Errorf(`%v is missing for durable storage type`, storagePath)
	}

	if spec.StorageClassName != nil {
		if _, err := client.StorageV1beta1().StorageClasses().Get(context.TODO(), *spec.StorageClassName, metav1.GetOptions{}); err != nil {
			if kerr.IsNotFound(err) {
				return fmt.Errorf(`%v.storageClassName "%v" not found`, storagePath, *spec.StorageClassName)
			}
			return err
		}
	}

	if val, found := spec.Resources.Requests[core.ResourceStorage]; found {
		if val.Value() <= 0 {
			return errors.New("invalid ResourceStorage request")
		}
	} else {
		return errors.New("missing ResourceStorage request")
	}

	return nil
}

// ValidateMonitorSpec validates the Monitoring spec after all the defaulting is done.
func ValidateMonitorSpec(agent *mona.AgentSpec) error {
	if agent.Agent == "" {
		return fmt.Errorf(`object 'Agent' is missing in '%+v'`, agent)
	}

	if !mona.IsKnownAgentType(agent.Agent) {
		return fmt.Errorf("unknown monitoring agent type %s", agent.Agent)
	}

	if agent.Agent.Vendor() == mona.VendorPrometheus {
		if agent.Prometheus != nil &&
			agent.Prometheus.Exporter.Port >= 1024 &&
			agent.Prometheus.Exporter.Port <= 65535 {
			return nil
		}
		return fmt.Errorf(`invalid 'monitor.prometheus' in '%+v'. prometheus.exporter.port value must be between 1024 and 65535, inclusive`, agent)
	}

	return fmt.Errorf(`invalid 'Agent' in '%+v'`, agent)
}

func ValidateEnvVar(envs []core.EnvVar, forbiddenEnvs []string, resourceType string) error {
	for _, env := range envs {
		present, _ := arrays.Contains(forbiddenEnvs, env.Name)
		if present {
			return fmt.Errorf("environment variable %s is forbidden to use in %s spec", env.Name, resourceType)
		}
	}
	return nil
}
