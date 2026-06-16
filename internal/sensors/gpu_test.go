package sensors

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pq/docker-server/host-agent/internal/runner"
)

func TestGPU_Probe_Auto_NoNvidiaSmi(t *testing.T) {
	r := runner.NewFakeRunner()
	// nvidia-smi -L returns "" + nil error → no GPU detected.
	g := NewGPU(r)
	label, fatal := g.Probe(context.Background(), "auto")
	if fatal {
		t.Fatalf("auto mode shouldn't be fatal: %s", label)
	}
	if g.Enabled {
		t.Error("auto with no GPU should disable")
	}
}

func TestGPU_Probe_TrueFailsFatally(t *testing.T) {
	r := runner.NewFakeRunner()
	g := NewGPU(r)
	_, fatal := g.Probe(context.Background(), "true")
	if !fatal {
		t.Error("true mode without GPU should be fatal")
	}
}

func TestGPU_Read_PassiveAndActive(t *testing.T) {
	r := runner.NewFakeRunner()
	// 2 passive cards, 1 active card.
	r.Set("nvidia-smi", []string{
		"--query-gpu=index,temperature.gpu,fan.speed",
		"--format=csv,noheader,nounits",
	}, runner.FakeResponse{
		Output: "0, 72, [N/A]\n1, 75, [NotSupported]\n2, 65, 80\n",
	})

	g := &GPU{Runner: r, Enabled: true}
	passive, active, activeFan, deets, ok := g.Read(context.Background())
	if !ok {
		t.Fatal("Read ok=false")
	}
	if passive != 75 {
		t.Errorf("passive max: got %d want 75", passive)
	}
	if active != 65 {
		t.Errorf("active max: got %d want 65", active)
	}
	if activeFan != 80 {
		t.Errorf("active fan max: got %d want 80", activeFan)
	}
	for _, want := range []string{"Gp0:72", "Gp1:75", "Ga2:65@80%"} {
		if !contains(deets, want) {
			t.Errorf("details missing %q: %s", want, deets)
		}
	}
}

func TestGPU_Read_DisabledReturnsNotOk(t *testing.T) {
	g := &GPU{Enabled: false}
	_, _, _, _, ok := g.Read(context.Background())
	if ok {
		t.Error("disabled GPU should return ok=false")
	}
}

// TestGPU_Probe_DetectsAndSummarizes covers the GPU-present branches of
// Probe (auto + true) and the summarize() name query.
func TestGPU_Probe_DetectsAndSummarizes(t *testing.T) {
	for _, mode := range []string{"auto", "true"} {
		r := runner.NewFakeRunner()
		r.Set("nvidia-smi", []string{"-L"}, runner.FakeResponse{
			Output: "GPU 0: Tesla P4 (UUID: GPU-abc)\nGPU 1: Tesla P40 (UUID: GPU-def)\n",
		})
		r.Set("nvidia-smi", []string{"--query-gpu=name", "--format=csv,noheader"}, runner.FakeResponse{
			Output: "Tesla P4\nTesla P40\n",
		})
		g := NewGPU(r)
		label, fatal := g.Probe(context.Background(), mode)
		if fatal {
			t.Fatalf("%s: unexpected fatal: %s", mode, label)
		}
		if !g.Enabled {
			t.Errorf("%s: GPU should be enabled when present", mode)
		}
		if !strings.Contains(label, "Tesla P4") || !strings.Contains(label, "Tesla P40") {
			t.Errorf("%s: label should summarize GPU names, got %q", mode, label)
		}
	}
}

// TestGPU_Summarize_QueryError returns "" when the name query fails.
func TestGPU_Summarize_QueryError(t *testing.T) {
	r := runner.NewFakeRunner()
	r.Set("nvidia-smi", []string{"-L"}, runner.FakeResponse{Output: "GPU 0: Tesla P4\n"})
	// Name query errors → summarize() hits its err!=nil branch and returns "".
	r.Set("nvidia-smi", []string{"--query-gpu=name", "--format=csv,noheader"},
		runner.FakeResponse{Err: errors.New("nvidia-smi crashed")})
	g := NewGPU(r)
	label, fatal := g.Probe(context.Background(), "auto")
	if fatal {
		t.Fatalf("unexpected fatal: %s", label)
	}
	if !g.Enabled {
		t.Error("GPU should be enabled (it was detected)")
	}
	// summarize returned "" → label ends with "detected: ".
	if !strings.HasSuffix(label, "detected: ") {
		t.Errorf("label with failed summarize should end 'detected: ', got %q", label)
	}
}
