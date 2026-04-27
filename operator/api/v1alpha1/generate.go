//go:generate go run sigs.k8s.io/controller-tools/cmd/controller-gen crd paths=./... output:crd:dir=../../../helm/spire/crds
//go:generate sh -c "rm -f ../../../k8s/crds/*.yaml && cp ../../../helm/spire/crds/*.yaml ../../../k8s/crds/"

// +groupName=spire.awell.io
package v1alpha1
