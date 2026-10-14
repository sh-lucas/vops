package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/sh-lucas/vops/internal/podman"
	"github.com/sh-lucas/vops/internal/proxy"
)

func TestCanaryRemovalConfirmation(t *testing.T) {
	for _, applied := range []bool{false, true} {
		t.Run(map[bool]string{false: "unreachable", true: "lost reply"}[applied], func(t *testing.T) {
			dir := t.TempDir()
			marker, bin := filepath.Join(dir, "removed"), filepath.Join(dir, "podman")
			if err := os.WriteFile(bin, []byte("#!/bin/sh\nif [ \"$1\" = rm ]; then touch \"$CANARY_REMOVED\"; fi\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("VOPS_PODMAN", bin)
			t.Setenv("CANARY_REMOVED", marker)
			oldDrain := Drain
			Drain = 0
			defer func() { Drain = oldDrain }()
			routes, remote := proxy.NewTable(), proxy.NewTable()
			routes.SetRoutes([]proxy.Route{{Project: "shop", Service: "web", Domains: []string{"shop.test"}, Backends: []string{"127.0.0.1:1"}, Canary: &proxy.Canary{ID: 7, Weight: 10, Backends: []string{"127.0.0.1:2"}}}})
			remote.SetRoutes(routes.Routes())
			routes.OnChange = func() error {
				if applied {
					remote.SetRoutes(routes.Routes())
				}
				return errors.New("proxy did not confirm")
			}
			e := &Engine{Routes: routes}
			pp := &ProjectPlan{Path: "shop", Canaries: []CanaryState{{Service: "web", Action: "remove", Containers: []podman.Container{{ID: "old", Names: []string{"canary"}}}}}}
			e.syncCanaries(context.Background(), io.Discard, pp, func(string) bool { return true })
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("container removed without confirmation: %v", err)
			}
			routes.OnChange = func() error { remote.SetRoutes(routes.Routes()); return nil }
			e.syncCanaries(context.Background(), io.Discard, pp, func(string) bool { return true })
			if _, err := os.Stat(marker); err != nil || remote.Routes()[0].Canary != nil {
				t.Fatalf("confirmed removal did not complete: %v", err)
			}
		})
	}
}

func TestCanaryHousekeepingDoesNotReviveRejected(t *testing.T) {
	v := newEnv(t)
	ctx := context.Background()
	_, addr, push := ownRegistry(t, v)
	p, repo := v.ns+"/shop", v.ns+"/web"
	file := p + "/compose.yml"
	web := "x-vops: {release: canary}\nservices:\n  web:\n    image: " + addr + "/" + repo + ":latest\n    x-vops: {port: 8080, health: /health}\n"
	api := "  api:\n    image: " + addr + "/" + repo + ":latest\n    x-vops: {port: 8080, health: /health}\n"
	v.commit(map[string]string{"vops.yml": "domain: " + v.ns + ".test\n", file: web + api})
	push(repo, "latest", "one")
	v.mustApply()
	if err := v.e.Pushed(ctx, io.Discard, push(repo, "latest", "two")); err != nil {
		t.Fatal(err)
	}
	cs, err := podman.PS(ctx, LProject+"="+p, LRole+"=canary")
	if err != nil || len(cs) != 2 {
		t.Fatalf("setup: %v, %d canaries", err, len(cs))
	}
	var id int64
	for _, c := range cs {
		if c.Labels[LService] == "web" {
			if _, err := podman.Run(ctx, "stop", "-t", "0", c.ID); err != nil {
				t.Fatal(err)
			}
			fmt.Sscan(c.Labels[LRelease], &id)
		}
	}
	v.commit(map[string]string{file: web})
	v.e.CheckCanaries(ctx, io.Discard)
	r, _, err := v.e.DB.GetRelease(id)
	cs, psErr := podman.PS(ctx, LProject+"="+p, LRole+"=canary")
	if err != nil || psErr != nil || r.Status != "rejected" || len(cs) != 0 {
		t.Fatalf("rejected canary revived: release=%+v containers=%+v errors=%v/%v", r, cs, err, psErr)
	}
	for _, route := range v.e.Routes.Routes() {
		if route.Canary != nil {
			t.Fatal("rejected canary routed again")
		}
	}
}
