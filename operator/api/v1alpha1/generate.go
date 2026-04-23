//go:generate go run sigs.k8s.io/controller-tools/cmd/controller-gen crd paths=./... output:crd:dir=../../../helm/spire/crds

// +groupName=spire.awell.io
package v1alpha1
