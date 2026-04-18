package recovery

import (
	"strings"
	"testing"
)

func TestMechanicalRecipe_Validate(t *testing.T) {
	cases := []struct {
		name    string
		recipe  *MechanicalRecipe
		wantErr string
	}{
		{
			name:   "nil is valid (no recipe)",
			recipe: nil,
		},
		{
			name: "builtin with action is valid",
			recipe: &MechanicalRecipe{
				Kind:   RecipeKindBuiltin,
				Action: "rebase-onto-base",
			},
		},
		{
			name: "builtin with action and params is valid",
			recipe: &MechanicalRecipe{
				Kind:   RecipeKindBuiltin,
				Action: "reset-to-step",
				Params: map[string]string{"step_target": "implement"},
			},
		},
		{
			name: "builtin without action is invalid",
			recipe: &MechanicalRecipe{
				Kind: RecipeKindBuiltin,
			},
			wantErr: "builtin kind requires action",
		},
		{
			name: "builtin with steps is invalid",
			recipe: &MechanicalRecipe{
				Kind:   RecipeKindBuiltin,
				Action: "rebuild",
				Steps:  []MechanicalRecipe{{Kind: RecipeKindBuiltin, Action: "rebase-onto-base"}},
			},
			wantErr: "builtin kind must not have steps",
		},
		{
			name: "sequence with steps is valid",
			recipe: &MechanicalRecipe{
				Kind: RecipeKindSequence,
				Steps: []MechanicalRecipe{
					{Kind: RecipeKindBuiltin, Action: "rebase-onto-base"},
					{Kind: RecipeKindBuiltin, Action: "rebuild"},
				},
			},
		},
		{
			name: "sequence without steps is invalid",
			recipe: &MechanicalRecipe{
				Kind: RecipeKindSequence,
			},
			wantErr: "sequence kind requires at least one step",
		},
		{
			name: "sequence with action is invalid",
			recipe: &MechanicalRecipe{
				Kind:   RecipeKindSequence,
				Action: "nope",
				Steps:  []MechanicalRecipe{{Kind: RecipeKindBuiltin, Action: "rebuild"}},
			},
			wantErr: "sequence kind must not have action",
		},
		{
			name: "sequence with invalid nested step is invalid",
			recipe: &MechanicalRecipe{
				Kind: RecipeKindSequence,
				Steps: []MechanicalRecipe{
					{Kind: RecipeKindBuiltin}, // missing action
				},
			},
			wantErr: "step[0]",
		},
		{
			name:    "unknown kind is invalid",
			recipe:  &MechanicalRecipe{Kind: "bogus", Action: "x"},
			wantErr: `unknown kind "bogus"`,
		},
		{
			name:    "empty kind is invalid",
			recipe:  &MechanicalRecipe{Action: "x"},
			wantErr: "unknown kind",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.recipe.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestNewBuiltinRecipe(t *testing.T) {
	t.Run("empty name returns nil", func(t *testing.T) {
		if got := NewBuiltinRecipe("", map[string]string{"k": "v"}); got != nil {
			t.Fatalf("NewBuiltinRecipe(\"\") = %v, want nil", got)
		}
	})
	t.Run("valid name without params", func(t *testing.T) {
		r := NewBuiltinRecipe("rebuild", nil)
		if r == nil {
			t.Fatal("NewBuiltinRecipe returned nil")
		}
		if r.Kind != RecipeKindBuiltin {
			t.Errorf("Kind = %q, want %q", r.Kind, RecipeKindBuiltin)
		}
		if r.Action != "rebuild" {
			t.Errorf("Action = %q, want %q", r.Action, "rebuild")
		}
		if r.Params == nil || len(r.Params) != 0 {
			t.Errorf("Params = %v, want empty (non-nil) map", r.Params)
		}
		if err := r.Validate(); err != nil {
			t.Errorf("Validate() = %v, want nil", err)
		}
	})
	t.Run("params are copied (caller mutation does not leak)", func(t *testing.T) {
		params := map[string]string{"step_target": "implement"}
		r := NewBuiltinRecipe("reset-to-step", params)
		params["step_target"] = "mutated"
		if r.Params["step_target"] != "implement" {
			t.Errorf("Params[step_target] = %q, want %q (caller mutation leaked)", r.Params["step_target"], "implement")
		}
	})
}

func TestMarshalRecipe(t *testing.T) {
	t.Run("nil marshals to empty string", func(t *testing.T) {
		got, err := MarshalRecipe(nil)
		if err != nil {
			t.Fatalf("MarshalRecipe(nil) err = %v", err)
		}
		if got != "" {
			t.Errorf("MarshalRecipe(nil) = %q, want empty", got)
		}
	})
	t.Run("builtin marshals to JSON with kind and action", func(t *testing.T) {
		r := &MechanicalRecipe{Kind: RecipeKindBuiltin, Action: "rebase-onto-base"}
		got, err := MarshalRecipe(r)
		if err != nil {
			t.Fatalf("MarshalRecipe() err = %v", err)
		}
		if !strings.Contains(got, `"kind":"builtin"`) {
			t.Errorf("MarshalRecipe() = %q, want kind=builtin", got)
		}
		if !strings.Contains(got, `"action":"rebase-onto-base"`) {
			t.Errorf("MarshalRecipe() = %q, want action=rebase-onto-base", got)
		}
	})
}

func TestUnmarshalRecipe(t *testing.T) {
	t.Run("empty string returns nil, no error", func(t *testing.T) {
		got, err := UnmarshalRecipe("")
		if err != nil {
			t.Fatalf("UnmarshalRecipe(\"\") err = %v", err)
		}
		if got != nil {
			t.Errorf("UnmarshalRecipe(\"\") = %v, want nil", got)
		}
	})
	t.Run("valid builtin round-trips", func(t *testing.T) {
		orig := &MechanicalRecipe{
			Kind:   RecipeKindBuiltin,
			Action: "rebase-onto-base",
			Params: map[string]string{"base": "main"},
		}
		raw, err := MarshalRecipe(orig)
		if err != nil {
			t.Fatalf("MarshalRecipe err = %v", err)
		}
		got, err := UnmarshalRecipe(raw)
		if err != nil {
			t.Fatalf("UnmarshalRecipe err = %v", err)
		}
		if got == nil {
			t.Fatal("UnmarshalRecipe returned nil")
		}
		if got.Kind != orig.Kind || got.Action != orig.Action {
			t.Errorf("got = %+v, want = %+v", got, orig)
		}
		if got.Params["base"] != "main" {
			t.Errorf("Params[base] = %q, want main", got.Params["base"])
		}
	})
	t.Run("valid sequence round-trips", func(t *testing.T) {
		orig := &MechanicalRecipe{
			Kind: RecipeKindSequence,
			Steps: []MechanicalRecipe{
				{Kind: RecipeKindBuiltin, Action: "rebase-onto-base"},
				{Kind: RecipeKindBuiltin, Action: "rebuild"},
			},
		}
		raw, err := MarshalRecipe(orig)
		if err != nil {
			t.Fatalf("MarshalRecipe err = %v", err)
		}
		got, err := UnmarshalRecipe(raw)
		if err != nil {
			t.Fatalf("UnmarshalRecipe err = %v", err)
		}
		if got.Kind != RecipeKindSequence || len(got.Steps) != 2 {
			t.Fatalf("got = %+v, want sequence with 2 steps", got)
		}
		if got.Steps[0].Action != "rebase-onto-base" || got.Steps[1].Action != "rebuild" {
			t.Errorf("Steps = %+v, want [rebase-onto-base, rebuild]", got.Steps)
		}
	})
	t.Run("malformed JSON returns error", func(t *testing.T) {
		_, err := UnmarshalRecipe("{not json")
		if err == nil {
			t.Fatal("UnmarshalRecipe(malformed) = nil, want error")
		}
		if !strings.Contains(err.Error(), "unmarshal mechanical recipe") {
			t.Errorf("err = %v, want unmarshal error", err)
		}
	})
	t.Run("structurally invalid recipe returns error", func(t *testing.T) {
		// Valid JSON, but Validate() fails (builtin missing action).
		_, err := UnmarshalRecipe(`{"kind":"builtin"}`)
		if err == nil {
			t.Fatal("UnmarshalRecipe(invalid) = nil, want validation error")
		}
		if !strings.Contains(err.Error(), "builtin kind requires action") {
			t.Errorf("err = %v, want validate error about missing action", err)
		}
	})
	t.Run("unknown kind returns error", func(t *testing.T) {
		_, err := UnmarshalRecipe(`{"kind":"bogus","action":"x"}`)
		if err == nil {
			t.Fatal("UnmarshalRecipe(unknown kind) = nil, want error")
		}
		if !strings.Contains(err.Error(), "unknown kind") {
			t.Errorf("err = %v, want unknown kind error", err)
		}
	})
}
