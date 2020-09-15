/*
Copyright (c) 2019 StackRox Inc.

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
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"istio.io/istio/pilot/cmd/pilot-agent/status"
	"k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	tlsDir      = `/run/secrets/tls`
	tlsCertFile = `tls.crt`
	tlsKeyFile  = `tls.key`
	image       = "registry.cn-hangzhou.aliyuncs.com/linkedcare/skywalking-agent:v2-jmx"
	//SKYWORKING_SERVER = "skywalking-oap.sre.svc.cluster.local:11800"
)

var (
	podResource       = metav1.GroupVersionResource{Version: "v1", Resource: "pods"}
	SKYWORKING_SERVER = os.Getenv("SKYWORKING_SERVER")
)

func addInitContainer(p *corev1.Pod) (patch patchOperation) {

	req := corev1.ResourceList{
		"cpu":    resource.MustParse("10m"),
		"memory": resource.MustParse("20Mi"),
	}

	lim := corev1.ResourceList{
		"cpu":    resource.MustParse("30m"),
		"memory": resource.MustParse("50Mi"),
	}

	vault := corev1.Container{
		Name:            "sky-working-init",
		Image:           image,
		ImagePullPolicy: "Always",

		Resources: corev1.ResourceRequirements{
			Requests: req,
			Limits:   lim,
		},
		VolumeMounts: []corev1.VolumeMount{
			corev1.VolumeMount{
				Name:      "skyworking-agent",
				MountPath: "/skyworking",
			},
		},
		Command: []string{"cp", "-rf", "/usr/local/agent", "/skyworking"},
	}
	p.Spec.InitContainers = append(p.Spec.InitContainers, vault)

	return patchOperation{
		Op:    "add",
		Path:  "/spec/initContainers",
		Value: p.Spec.InitContainers,
	}
}

func addSkyWorkingVolume(p *corev1.Pod) (patch patchOperation) {
	volume := corev1.Volume{
		Name: "skyworking-agent",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{
				Medium: "",
			},
		},
	}
	p.Spec.Volumes = append(p.Spec.Volumes, volume)

	return patchOperation{
		Op:    "add",
		Path:  "/spec/volumes",
		Value: p.Spec.Volumes,
	}
}

func addJvmLabels(p *corev1.Pod) (patch patchOperation) {
	p.Labels["jvm"] = "prometheus"

	return patchOperation{
		Op:    "add",
		Path:  "/metadata/labels",
		Value: p.Labels,
	}
}

type patchContainers struct {
	index        string
	volumeMounts []corev1.VolumeMount
	envs         []corev1.EnvVar
}

func addContainers(p *corev1.Pod) (patchs []patchOperation) {
	containers := []patchContainers{}
	volumeMount := corev1.VolumeMount{
		Name:      "skyworking-agent",
		MountPath: "/skyworking",
	}

	service := ""
	if svc, ok := p.GetLabels()["run"]; ok {
		service = svc
	}
	if svc, ok := p.GetLabels()["app"]; ok {
		service = svc
	}
	if svc, ok := p.Annotations["linkedcare.io/skyworking-service"]; ok {
		service = svc
	}
	skyworkingEnabled := true
	jvmEnabled := true

	if enabled, ok := p.Annotations["linkedcare.io/skyworking-enabled"]; ok {
		if enabled == "false" {
			skyworkingEnabled = false
		}
	}

	if enabled, ok := p.Annotations["linkedcare.io/jvm-enabled"]; ok {
		if enabled == "false" {
			jvmEnabled = false
		}
	}

	var pathsMap status.KubeAppProbers

	for _, container := range p.Spec.Containers {

		if container.Name == "istio-proxy" {
			for _, env := range container.Env {
				if env.Name == "ISTIO_KUBE_APP_PROBERS" {
					err := json.Unmarshal([]byte(env.Value), &pathsMap)
					if err != nil {
						fmt.Println("JsonToMapDemo err: ", err)
					}
					break
				}
			}
		}
	}

	for index, container := range p.Spec.Containers {
		if container.Name != "istio-proxy" {
			container.VolumeMounts = append(container.VolumeMounts, volumeMount)

			SKYWORKING_IGNORE := ""
			envLive := ""
			envRead := ""
			if container.LivenessProbe != nil {
				if container.LivenessProbe.HTTPGet != nil {
					envLive = container.LivenessProbe.HTTPGet.Path
				}
			}
			if container.ReadinessProbe != nil {
				if container.ReadinessProbe.HTTPGet != nil {
					envRead = container.ReadinessProbe.HTTPGet.Path
				}
			}

			if envLive != "" {
				if v, ok := pathsMap[envLive]; ok {
					envLive = v.HTTPGet.Path
				}
			}

			if envRead != "" {
				if v, ok := pathsMap[envRead]; ok {
					envRead = v.HTTPGet.Path
				}
			}

			if envLive != "" && envRead != "" {
				SKYWORKING_IGNORE = envLive

				if envLive != envRead {
					SKYWORKING_IGNORE = envLive + "," + envRead
				}
			}

			if envLive != "" && envRead == "" {
				SKYWORKING_IGNORE = envLive
			}

			if envLive == "" && envRead != "" {
				SKYWORKING_IGNORE = envRead
			}

			if SKYWORKING_IGNORE != "" {
				SKYWORKING_IGNORE = "-Dskywalking.trace.ignore_path=" + SKYWORKING_IGNORE
			}

			jvmArg := ""
			skyArg := ""

			if skyworkingEnabled {
				skyArg = SKYWORKING_IGNORE + " -javaagent:/skyworking/agent/skywalking-agent.jar=agent.service_name=" + service + ",collector.backend_service=" + SKYWORKING_SERVER
			}
			if jvmEnabled {
				jvmArg = " -javaagent:/skyworking/agent/jmx/jmx_prometheus_javaagent-0.13.0.jar=65533:/skyworking/agent/jmx/config.yaml"
			}

			SKYWORKING_ARGES := skyArg + jvmArg

			argesEnv := corev1.EnvVar{
				Name:  "SKYWORKING_ARGES",
				Value: SKYWORKING_ARGES,
			}
			container.Env = append(container.Env, argesEnv)

			containers = append(containers, patchContainers{
				index:        strconv.Itoa(index),
				volumeMounts: container.VolumeMounts,
				envs:         container.Env,
			})
		}
	}

	for _, m := range containers {
		patchs = append(patchs, patchOperation{
			Op:    "add",
			Path:  "/spec/containers/" + m.index + "/volumeMounts",
			Value: m.volumeMounts,
		})
		patchs = append(patchs, patchOperation{
			Op:    "add",
			Path:  "/spec/containers/" + m.index + "/env",
			Value: m.envs,
		})
	}

	return patchs
}

func applySkyWorking(req *v1beta1.AdmissionRequest) ([]patchOperation, error) {
	// This handler should only get called on Pod objects as per the MutatingWebhookConfiguration in the YAML file.
	// However, if (for whatever reason) this gets invoked on an object of a different kind, issue a log message but
	// let the object request pass through otherwise.
	if req.Resource != podResource {
		log.Printf("expect resource to be %s", podResource)
		return nil, nil
	}

	// Parse the Pod object.
	raw := req.Object.Raw
	pod := corev1.Pod{}
	if _, _, err := universalDeserializer.Decode(raw, nil, &pod); err != nil {
		return nil, fmt.Errorf("could not deserialize pod object: %v", err)
	}

	podCopy := pod.DeepCopy()

	var patches []patchOperation

	if inject, ok := pod.Annotations["linkedcare.io/skyworking-injection"]; ok {
		if inject == "true" {
			patches = append(patches, addInitContainer(podCopy))
			patches = append(patches, addSkyWorkingVolume(podCopy))
			patches = append(patches, addContainers(podCopy)...)
		}
	}

	if jvmEnable, ok := pod.Annotations["linkedcare.io/jvm-enabled"]; ok {
		if jvmEnable == "true" {
			patches = append(patches, addJvmLabels(podCopy))
		}
	}
	return patches, nil

}

func main() {
	certPath := filepath.Join(tlsDir, tlsCertFile)
	keyPath := filepath.Join(tlsDir, tlsKeyFile)

	mux := http.NewServeMux()
	mux.Handle("/mutate", admitFuncHandler(applySkyWorking))
	server := &http.Server{
		// We listen on port 8443 such that we do not need root privileges or extra capabilities for this server.
		// The Service object will take care of mapping this port to the HTTPS port 443.
		Addr:    ":8443",
		Handler: mux,
	}
	log.Fatal(server.ListenAndServeTLS(certPath, keyPath))
}
