package pkg

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Status capture all scheduled pods with reason why the estimation could not continue
type Status struct {
	// all pods
	Pods []corev1.Pod `json:"pods"`
	// all nodes
	Nodes map[string]corev1.Node `json:"nodes"`
	// for ce
	PodsForEstimation []*corev1.Pod `json:"pods_for_estimation"`
	// for cc
	NodesToScaleDown []string `json:"nodes_to_scale_down"`
	StopReason       string   `json:"stop_reason"`
}

// Framework need to be implemented by all scheduler framework
type Framework interface {
	Run() error
	InitTheWorld(objs ...runtime.Object) error
	CreatePod(pod *corev1.Pod) error
	UpdateEstimationPods(pod ...*corev1.Pod)
	UpdateNodesToScaleDown(nodeName string)
	Status() Status
	GetPodsByNode(nodeName string) ([]*corev1.Pod, error)
	Stop(reason string) error
}

// Simulator need to be implemented by all simulator
type Simulator interface {
	Run() error
	Initialize(objs ...runtime.Object) error
	Report() Printer
}

type Printer interface {
	Print(verbose bool, format string) error
}
