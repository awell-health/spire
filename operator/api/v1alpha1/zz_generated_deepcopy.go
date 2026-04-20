package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime"
)

// DeepCopyObject implements runtime.Object for SpireAgent.
func (in *SpireAgent) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(SpireAgent)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies all properties into another SpireAgent.
func (in *SpireAgent) DeepCopyInto(out *SpireAgent) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *SpireAgentSpec) DeepCopyInto(out *SpireAgentSpec) {
	*out = *in
	if in.Capabilities != nil {
		out.Capabilities = make([]string, len(in.Capabilities))
		copy(out.Capabilities, in.Capabilities)
	}
	if in.Prefixes != nil {
		out.Prefixes = make([]string, len(in.Prefixes))
		copy(out.Prefixes, in.Prefixes)
	}
	if in.MaxApprentices != nil {
		v := *in.MaxApprentices
		out.MaxApprentices = &v
	}
	if in.Resources != nil {
		out.Resources = new(AgentResourceRequirements)
		in.Resources.DeepCopyInto(out.Resources)
	}
}

func (in *AgentResourceRequirements) DeepCopyInto(out *AgentResourceRequirements) {
	*out = *in
	if in.Requests != nil {
		out.Requests = make(map[string]string, len(in.Requests))
		for k, v := range in.Requests {
			out.Requests[k] = v
		}
	}
	if in.Limits != nil {
		out.Limits = make(map[string]string, len(in.Limits))
		for k, v := range in.Limits {
			out.Limits[k] = v
		}
	}
}

func (in *SpireAgentStatus) DeepCopyInto(out *SpireAgentStatus) {
	*out = *in
	if in.CurrentWork != nil {
		out.CurrentWork = make([]string, len(in.CurrentWork))
		copy(out.CurrentWork, in.CurrentWork)
	}
}

// DeepCopyObject implements runtime.Object for SpireAgentList.
func (in *SpireAgentList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(SpireAgentList)
	in.DeepCopyInto(out)
	return out
}

func (in *SpireAgentList) DeepCopyInto(out *SpireAgentList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]SpireAgent, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

// DeepCopyObject implements runtime.Object for SpireWorkload.
func (in *SpireWorkload) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(SpireWorkload)
	in.DeepCopyInto(out)
	return out
}

func (in *SpireWorkload) DeepCopyInto(out *SpireWorkload) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	out.Status = in.Status
}

func (in *SpireWorkloadSpec) DeepCopyInto(out *SpireWorkloadSpec) {
	*out = *in
	if in.Prefixes != nil {
		out.Prefixes = make([]string, len(in.Prefixes))
		copy(out.Prefixes, in.Prefixes)
	}
}

// DeepCopyObject implements runtime.Object for SpireWorkloadList.
func (in *SpireWorkloadList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(SpireWorkloadList)
	in.DeepCopyInto(out)
	return out
}

func (in *SpireWorkloadList) DeepCopyInto(out *SpireWorkloadList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]SpireWorkload, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

// DeepCopyObject implements runtime.Object for SpireConfig.
func (in *SpireConfig) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(SpireConfig)
	in.DeepCopyInto(out)
	return out
}

func (in *SpireConfig) DeepCopyInto(out *SpireConfig) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	out.Status = in.Status
}

func (in *SpireConfigSpec) DeepCopyInto(out *SpireConfigSpec) {
	*out = *in
	out.DoltHub = in.DoltHub
	out.Polling = in.Polling
	if in.Tokens != nil {
		out.Tokens = make(map[string]TokenRef, len(in.Tokens))
		for k, v := range in.Tokens {
			out.Tokens[k] = v
		}
	}
	if in.Routing != nil {
		out.Routing = make([]RoutingRule, len(in.Routing))
		for i := range in.Routing {
			in.Routing[i].DeepCopyInto(&out.Routing[i])
		}
	}
}

func (in *RoutingRule) DeepCopyInto(out *RoutingRule) {
	*out = *in
	if in.Match != nil {
		out.Match = make(map[string]string, len(in.Match))
		for k, v := range in.Match {
			out.Match[k] = v
		}
	}
}

// DeepCopyObject implements runtime.Object for SpireConfigList.
func (in *SpireConfigList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(SpireConfigList)
	in.DeepCopyInto(out)
	return out
}

func (in *SpireConfigList) DeepCopyInto(out *SpireConfigList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]SpireConfig, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}
