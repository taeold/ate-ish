package main

import (
	"slices"
	"time"
)

// The bucket layout. The actor record is the only truth about placement.
func atespaceKey(as string) string    { return "atespaces/" + as + "/atespace.json" }
func templateKey(as, t string) string { return "atespaces/" + as + "/templates/" + t + ".json" }
func actorKey(as, a string) string    { return "atespaces/" + as + "/actors/" + a + ".json" }
func snapshotKey(as, a string) string { return "atespaces/" + as + "/snapshots/" + a + ".tar" }
func tagKey(as, t string) string      { return "atespaces/" + as + "/tags/" + t + ".tar" }
func leaseKey(id string) string       { return "workers/" + id + ".json" }

const (
	running   = "running"
	suspended = "suspended"
)

type Actor struct {
	Template string `json:"template"`
	Status   string `json:"status"`
	// Owner is the worker id while running.
	Owner string `json:"owner,omitempty"`
	// Generation counts placements. A worker serves only the generation it was given.
	Generation   int64         `json:"generation"`
	EgressPolicy *EgressPolicy `json:"egressPolicy,omitempty"`
	Created      time.Time     `json:"created"`
}

// Lease is a worker's heartbeat. Hosted is a cache of what it runs.
type Lease struct {
	Addr     string    `json:"addr"`
	Capacity int       `json:"capacity"`
	Hosted   []string  `json:"hosted"`
	Expires  time.Time `json:"expires"`
}

func (l *Lease) live() bool { return l != nil && time.Now().Before(l.Expires) }

// hosts says whether the worker's live lease lists the actor ("as/name").
func (l *Lease) hosts(key string) bool { return l.live() && slices.Contains(l.Hosted, key) }

// Template is the part of substrate's ActorTemplate that ate-ish reads.
// Other fields are stored and ignored.
type Template struct {
	Metadata struct {
		Atespace string `json:"atespace"`
		Name     string `json:"name"`
	} `json:"metadata"`
	Containers []Container `json:"containers"`
	Volumes    []Volume    `json:"volumes"`
	// PreferredFidelity selects CheckpointWorkload/RestoreWorkload's
	// fidelity: "MEMORY" checkpoints the sandbox's process memory; anything
	// else (including empty) uses "VOLUMES", durable dirs only, which is
	// what the worker's RunWorkload-only (no snapshot yet) path also uses.
	PreferredFidelity string `json:"preferredFidelity,omitempty"`
}

// Container describes the one container every template carries.
type Container struct {
	// Name becomes the container's own OCI bundle subdirectory and the
	// ateworkerpb.Container name; required by the vendored worker.
	Name string `json:"name"`
	// Image, when set, is the actor's rootfs: exported once per node into
	// <dir>/rootfs/<digest> and used as the runsc overlay's lowerdir
	// (bundle.go's imageRootfsDir), instead of the node's own filesystem.
	// Empty means the pre-stage-8 behaviour: the node's own filesystem
	// (bundle.go's rootfsLowerDir), which gate.sh's own demos still use.
	Image   string   `json:"image,omitempty"`
	Command []string `json:"command"`
	Args    []string `json:"args"`
	Env     []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"env"`
	WakeupProbe struct {
		HTTPGet struct {
			Path string `json:"path"`
			Port int    `json:"port"`
		} `json:"httpGet"`
		TimeoutSeconds int `json:"timeoutSeconds"`
	} `json:"wakeupProbe"`
	VolumeMounts []struct {
		Name      string `json:"name"`
		MountPath string `json:"mountPath"`
	} `json:"volumeMounts"`
}

type Volume struct {
	Name       string    `json:"name"`
	DurableDir *struct{} `json:"durableDir"`
	SystemInfo *struct {
		DataSources []struct {
			TrustBundle *struct {
				Path string `json:"path"`
			} `json:"trustBundle"`
		} `json:"dataSources"`
	} `json:"systemInfo"`
}

type EgressPolicy struct {
	Rules []struct {
		HTTPS struct {
			Hostnames []string `json:"hostnames"`
			Effects   struct {
				ReplaceHeaders []ReplaceHeader `json:"replaceHeaders"`
			} `json:"effects"`
		} `json:"https"`
	} `json:"rules"`
}

type ReplaceHeader struct {
	Header        string `json:"header"`
	Prefix        string `json:"prefix,omitempty"`
	CredentialURI string `json:"credentialUri"`
}
