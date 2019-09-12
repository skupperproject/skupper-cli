/*
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

package router

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/skupperproject/skupper-cli/pkg/kube"
)

type RouterNode struct {
	Id      string  `json:"id"`
	Name    string  `json:"name"`
	NextHop string  `json:"nextHop"`
}

type ConnectedSites struct {
	Direct   int
	Indirect int
	Total    int
}

func GetConnectedSites(namespace string, clientset *kubernetes.Clientset, config *restclient.Config) (ConnectedSites, error) {
	result := ConnectedSites{}
	nodes, err := GetNodes(namespace, clientset, config)
	if err == nil {
		for _, n := range nodes {
			if n.NextHop == "" {
				result.Direct++
				result.Total++
			} else if n.NextHop != "(self)" {
				result.Indirect++
				result.Total++
			}
		}
	}
	return result, err
}

func GetNodes(namespace string, clientset *kubernetes.Clientset, config *restclient.Config) ([]RouterNode, error) {
	pod, err := kube.GetReadyPod(namespace, clientset, "router")
	if err != nil {
		return nil, err
	}

	command := []string{
		"qdmanage",
		"query",
		"--type",
		"node",
	}
	var stdout io.Writer

	buffer := bytes.Buffer{}
	stdout = bufio.NewWriter(&buffer)


	restClient, err := restclient.RESTClientFor(config)
	if err != nil {
		panic(err)
	}

	req := restClient.Post().
		Resource("pods").
		Name(pod.Name).
		Namespace(pod.Namespace).
		SubResource("exec")
	req.VersionedParams(&corev1.PodExecOptions{
		Container: pod.Spec.Containers[0].Name,
		Command:   command,
		Stdin:     false,
		Stdout:    true,
		Stderr:    false,
		TTY:       false,
	}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		panic(err)
	}
	err = exec.Stream(remotecommand.StreamOptions{
		Stdin:             nil,
		Stdout:            stdout,
		Stderr:            nil,
	})
	if err != nil {
		return nil, err
	} else {
		results := []RouterNode{}
		err = json.Unmarshal(buffer.Bytes(), &results)
		if err != nil {
			fmt.Println("Failed to parse JSON:", err, buffer.String())
			return nil, err
		} else {
			return results, nil
		}
	}
}
