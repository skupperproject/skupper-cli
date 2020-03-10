package kube_test

import (
	"errors"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
	"testing"

	. "github.com/skupperproject/skupper-cli/pkg/kube"
)

var (
	testPod corev1.Pod

	podConditionScheduledFalse = corev1.PodCondition{Type: corev1.PodScheduled,
		Status: corev1.ConditionFalse,
	}
	podConditionScheduledTrue = corev1.PodCondition{Type: corev1.PodScheduled,
		Status: corev1.ConditionTrue,
	}
	podConditionReadyTrue = corev1.PodCondition{Type: corev1.PodReady,
		Status: corev1.ConditionTrue,
	}
	podConditionReadyFalse = corev1.PodCondition{Type: corev1.PodReady,
		Status: corev1.ConditionFalse,
	}
)

func TestIsPodReady(t *testing.T) {
	testStatus := func(ready bool, conditions []corev1.PodCondition) {
		testPod.Status.Conditions = conditions
		assert.Equal(t, IsPodReady(&testPod), ready)
	}

	testStatus(true, []corev1.PodCondition{podConditionScheduledFalse, podConditionReadyTrue})
	testStatus(true, []corev1.PodCondition{podConditionReadyTrue, podConditionScheduledFalse})
	testStatus(false, []corev1.PodCondition{podConditionScheduledFalse, podConditionReadyFalse})
	testStatus(false, []corev1.PodCondition{podConditionScheduledTrue, podConditionReadyFalse})
}

func TestIsPodRunning(t *testing.T) {
	podPhaseToRunning := map[corev1.PodPhase]bool{
		corev1.PodPending:   false,
		corev1.PodRunning:   true,
		corev1.PodSucceeded: false,
		corev1.PodFailed:    false,
		corev1.PodUnknown:   false,
	}
	testPhase := func(phase corev1.PodPhase, expectedIsRunning bool) {
		testPod.Status.Phase = phase
		if IsPodRunning(&testPod) != expectedIsRunning {
			t.Error("returned", !expectedIsRunning, "expected", expectedIsRunning)
		}
	}
	for phase := range podPhaseToRunning {
		testPhase(phase, podPhaseToRunning[phase])
	}
}

func TestGetReadyPod(t *testing.T) {
	assert := assert.New(t)
	namespace := "namespace"
	labelKey := "skupper.io/component"
	readyPodConditions := []corev1.PodCondition{podConditionReadyTrue}
	notReadyPodConditions := []corev1.PodCondition{podConditionReadyFalse}
	var kubeClient *fake.Clientset

	injectPod := func(name string, namespace string, component string, conditions []corev1.PodCondition) {
		var pod corev1.Pod
		pod.Labels = map[string]string{
			labelKey: component,
		}
		pod.Name = name
		pod.Namespace = namespace
		pod.Status.Conditions = conditions
		kubeClient.CoreV1().Pods(namespace).Create(&pod)
	}

	kubeClient = fake.NewSimpleClientset() //clean namespace
	injectPod("podA", namespace, "comp", readyPodConditions)
	injectPod("podB", namespace, "comp", notReadyPodConditions)
	pod, err := GetReadyPod(namespace, kubeClient, "comp")
	assert.Equal(err, nil)
	assert.Equal(pod.Name, "podA")

	kubeClient = fake.NewSimpleClientset() //clean namespace
	injectPod("podA", namespace, "XXXX", readyPodConditions)
	injectPod("podB", namespace, "comp", notReadyPodConditions)
	pod, err = GetReadyPod(namespace, kubeClient, "comp")
	assert.Equal(errors.New("Not ready"), err)

	kubeClient = fake.NewSimpleClientset() //clean namespace
	injectPod("podA", namespace, "XXXX", readyPodConditions)
	injectPod("podB", namespace, "AAAA", notReadyPodConditions)
	pod, err = GetReadyPod(namespace, kubeClient, "comp")
	assert.Equal(errors.New("Not found"), err)

	kubeClient = fake.NewSimpleClientset() //clean namespace
	injectPod("podA", namespace, "comp", notReadyPodConditions)
	injectPod("podB", namespace, "comp", readyPodConditions)
	injectPod("podC", namespace, "comp", readyPodConditions)
	pod, err = GetReadyPod(namespace, kubeClient, "comp")
	assert.Equal(err, nil)
	assert.Equal(pod.Name, "podB")
}
