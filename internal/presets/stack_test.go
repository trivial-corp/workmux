package presets

import (
	"strings"
	"testing"

	"github.com/trivial-corp/workmux/internal/config"
	"github.com/trivial-corp/workmux/internal/term"
)

func stackDeps(running, next string) Deps {
	cfg := &config.Config{Root: "/repo", Name: "proj"}
	cfg.Stack = &config.Stack{
		Compose: "compose.yaml",
		Slots:   "proj{n}",
		Commands: map[string]string{
			"up":      "docker compose -p {slot} -f {compose} up -d --build",
			"stop":    "docker compose -p {slot} -f {compose} down --remove-orphans",
			"restart": "docker compose -p {slot} -f {compose} restart",
		},
	}
	return Deps{
		Cfg:      cfg,
		SlotFor:  func(string) string { return running },
		NextSlot: func() string { return next },
	}
}

// Starting an app is minutes of build output whose only interesting part is the reason
// it stopped. Run as a request it was a toast and a silent wait, so a failure looked
// exactly like a slow success.
func TestStackActionRunsAsASessionYouCanWatch(t *testing.T) {
	d := stackDeps("", "proj2")
	spec, err := d.Spec(term.KindStack, "/repo/wt/feature", "up")
	if err != nil {
		t.Fatal(err)
	}
	// One word for exec: `exec (…); s=$?` execs the subshell and drops the rest.
	if !strings.HasPrefix(spec.Command, "sh -c '") {
		t.Errorf("the script has to survive being exec'd: %q", spec.Command)
	}
	if !strings.Contains(spec.Command, "docker compose -p proj2") {
		t.Errorf("the command should act on the next free slot: %q", spec.Command)
	}
	if !strings.Contains(spec.Command, "up -d --build") {
		t.Errorf("and be the project's own up command: %q", spec.Command)
	}
	if spec.Title != "up proj2" {
		t.Errorf("title = %q", spec.Title)
	}
	// Compose says plenty when it fails and nothing when it works.
	if !strings.Contains(spec.Command, "done") || !strings.Contains(spec.Command, "failed") {
		t.Errorf("the pane must end by saying whether it worked: %q", spec.Command)
	}
}

func TestStackActionUsesTheSlotAlreadyRunningHere(t *testing.T) {
	d := stackDeps("proj7", "proj2")
	for _, action := range []string{"stop", "restart"} {
		spec, err := d.Spec(term.KindStack, "/repo/wt/feature", action)
		if err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		if !strings.Contains(spec.Command, "-p proj7") {
			t.Errorf("%s should act on what is running here, not the next free slot: %q",
				action, spec.Command)
		}
	}
}

func TestStackActionRefusesWhatItCannotDo(t *testing.T) {
	d := stackDeps("", "")
	if _, err := d.Spec(term.KindStack, "/repo/wt/feature", "sideways"); err == nil {
		t.Error("an action that isn't one of the three must be refused")
	}
	// Nothing running here and no free slot to take: say so rather than run docker
	// with an empty project name, which would act on every unnamed container.
	if _, err := d.Spec(term.KindStack, "/repo/wt/feature", "stop"); err == nil {
		t.Error("stopping nothing must be refused")
	}
}
