package agent

// The dependency rules of the packages under pkg/agent, checked by
// imports_test.go. Paths are relative to the module.

// The packages under pkg/agent and the shared packages they may use.
const (
	pkgRoot      = "pkg/agent"
	pkgPlan      = "pkg/agent/plan"
	pkgHost      = "pkg/agent/host"
	pkgDocker    = "pkg/agent/host/docker"
	pkgSystem    = "pkg/agent/host/system"
	pkgProbe     = "pkg/agent/host/probe"
	pkgResources = "pkg/agent/host/resources"
	pkgRegistry  = "pkg/agent/registry"
	pkgLink      = "pkg/agent/link"
	pkgLease     = "pkg/agent/lease"
	pkgLocal     = "pkg/agent/local"
	pkgLogins    = "pkg/agent/logins"
	pkgAgenttest = "pkg/agent/agenttest"
	pkgProtocol  = "pkg/protocol"
	pkgIdentity  = "pkg/identity"
	pkgImageref  = "pkg/imageref"
)

// mayImport lists, per package, the packages of this module its non-test
// files may import.
var mayImport = map[string][]string{
	// The root wires everything but test support.
	pkgRoot: {
		pkgPlan, pkgHost, pkgRegistry, pkgLink,
		pkgLease, pkgLocal, pkgLogins, pkgProtocol, pkgIdentity, pkgImageref,
	},
	pkgPlan:   {pkgProtocol, pkgImageref},
	pkgLogins: {pkgProtocol},
	// Only host imports its sub-packages; they import no package of
	// pkg/agent.
	pkgHost:      {pkgDocker, pkgSystem, pkgProbe, pkgResources, pkgProtocol},
	pkgDocker:    {pkgProtocol, pkgImageref},
	pkgSystem:    {pkgProtocol},
	pkgProbe:     {pkgProtocol},
	pkgResources: {pkgProtocol},
	pkgRegistry:  {pkgProtocol, pkgImageref},
	pkgLink:      {pkgProtocol, pkgIdentity},
	pkgLease:     {pkgHost, pkgPlan, pkgProtocol},
	pkgLocal:     {pkgPlan, pkgProtocol},
	pkgAgenttest: {
		pkgHost, pkgRegistry, pkgProtocol, pkgIdentity, pkgImageref,
	},
}

// Standard-library and third-party imports that only some packages may use.
var (
	// purePackages do no I/O, read no clock and use no third-party package.
	purePackages = []string{pkgPlan, pkgLogins}
	// planForbids are the standard packages that do I/O; the pure packages
	// use none.
	planForbids = []string{"os", "net", "syscall", "io/ioutil", "io/fs", "path/filepath"}
	// planClock are the calls of package time that read the clock or wait;
	// the pure packages get the time as an argument.
	planClock = []string{
		"time.Now", "time.Since", "time.Until", "time.Sleep", "time.After", "time.NewTimer",
		"time.Tick", "time.NewTicker", "time.AfterFunc",
	}
	// onlyIn maps import prefixes to the one package that may use them.
	onlyIn = map[string]string{
		"github.com/moby/":           pkgDocker,
		"github.com/containerd/":     pkgDocker,
		"github.com/opencontainers/": pkgDocker,
		"os/exec":                    pkgSystem,
	}
	// httpAllowed may import net/http.
	httpAllowed = []string{pkgLink, pkgDocker, pkgProbe, pkgLocal, pkgAgenttest}
)

// hostBinary runs as root on every host. Its released dependency closure
// (linux/amd64 and linux/arm64, CGO_ENABLED=0, with and without the webui tag)
// links nothing under hostForbids and no module outside hostModules.
const hostBinary = "./cmd/upgradoor"

// hostForbids are the packages of this module (each with everything below
// it) the host binary never links.
var hostForbids = []struct{ path, what string }{
	{"pkg/controller", "the controller"},
	{"web", "the embedded web UI"},
	{pkgAgenttest, "test support"},
	{"test", "the test tools"},
}

// hostModules are the modules the host binary may link.
var hostModules = []string{
	// this module
	"github.com/ethpandaops/upgradoor",
	// logging, CLI, configuration files, syscalls
	"github.com/sirupsen/logrus",
	"github.com/spf13/cobra",
	"github.com/spf13/pflag",
	"gopkg.in/yaml.v3",
	"golang.org/x/sys",
	// image references (pkg/imageref and the Docker client)
	"github.com/distribution/reference",
	"github.com/opencontainers/go-digest",
	// the Docker client and its API types
	"github.com/moby/moby/client",
	"github.com/moby/moby/api",
	"github.com/moby/docker-image-spec",
	"github.com/opencontainers/image-spec",
	"github.com/containerd/errdefs",
	"github.com/containerd/errdefs/pkg",
	"github.com/docker/go-connections",
	"github.com/docker/go-units",
	// the Docker client's HTTP instrumentation (otelhttp) and what it pulls in
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp",
	"go.opentelemetry.io/otel",
	"go.opentelemetry.io/otel/metric",
	"go.opentelemetry.io/otel/trace",
	"go.opentelemetry.io/auto/sdk",
	"github.com/felixge/httpsnoop",
	"github.com/go-logr/logr",
	"github.com/go-logr/stdr",
}
