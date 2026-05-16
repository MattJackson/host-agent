package sensors

import (
	"context"
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
