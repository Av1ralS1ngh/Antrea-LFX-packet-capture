package packetcapture

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PacketCapture defines the PacketCapture custom resource.
type PacketCapture struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              PacketCaptureSpec   `json:"spec"`
	Status            PacketCaptureStatus `json:"status,omitempty"`
}

// PacketCaptureSpec describes which Pods to capture and for how long.
type PacketCaptureSpec struct {
	PodSelector metav1.LabelSelector `json:"podSelector"`
	Timeout     metav1.Duration      `json:"timeout,omitempty"`
}

// PacketCaptureStatus reports capture state back to the user.
type PacketCaptureStatus struct {
	Phase        string `json:"phase,omitempty"`
	FileLocation string `json:"fileLocation,omitempty"`
	Message      string `json:"message,omitempty"`
	NodeName     string `json:"nodeName,omitempty"`
}
