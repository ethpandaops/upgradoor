package store

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// sealInventory sorts the lists as the protocol requires and sets the hash.
func sealInventory(t *testing.T, inv *protocol.Inventory) *protocol.Inventory {
	t.Helper()

	sort.Slice(inv.Packages, func(i, j int) bool {
		a, b := inv.Packages[i], inv.Packages[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}

		return a.Arch < b.Arch
	})
	sort.Slice(inv.Upgradable, func(i, j int) bool {
		a, b := inv.Upgradable[i], inv.Upgradable[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}

		return a.Arch < b.Arch
	})

	hash, err := inv.ComputeHash()
	require.NoError(t, err)

	inv.Hash = hash

	return inv
}

// newInventory returns an unsealed inventory with n packages named pkg-0000,
// pkg-0001, ... and every tenth of them upgradable.
func newInventory(n int) *protocol.Inventory {
	inv := &protocol.Inventory{
		CollectedAt: at(-30),
		OS: protocol.OSInfo{
			ID: "debian", VersionID: "13", Codename: "trixie",
			PrettyName: "Debian GNU/Linux 13 (trixie)", Arch: "amd64",
		},
		Kernels: protocol.KernelInfo{
			Running: "6.12.1-amd64", Newest: "6.12.2-amd64",
			Installed: []string{"6.12.1-amd64", "6.12.2-amd64"},
		},
		Packages:   make([]protocol.Package, 0, n),
		Upgradable: make([]protocol.Upgradable, 0, n/10+1),
	}

	for i := range n {
		name := fmt.Sprintf("pkg-%04d", i)
		inv.Packages = append(inv.Packages, protocol.Package{Name: name, Version: "1.0-1", Arch: "amd64"})

		if i%10 == 0 {
			inv.Upgradable = append(inv.Upgradable, protocol.Upgradable{
				Name: name, Arch: "amd64", Current: "1.0-1", Candidate: "1.0-2",
				Origin: "Debian-Security:13/stable-security", Security: i%20 == 0, Blocked: i%30 == 0,
			})
		}
	}

	return inv
}

// testInventory returns a sealed inventory with n packages. It does not take
// a testing.T so fixtures can be built for comparison anywhere.
func testInventory(n int) *protocol.Inventory {
	inv := newInventory(n)

	hash, err := inv.ComputeHash()
	if err != nil {
		panic(err)
	}

	inv.Hash = hash

	return inv
}

func TestInventoryRoundTrip(t *testing.T) {
	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)

			addHost(t, s, "node-1", nil)

			_, err := s.GetInventory(ctx, "node-1")
			require.ErrorIs(t, err, ErrNotFound, "a host without an inventory")
			_, err = s.GetInventory(ctx, "nope")
			require.ErrorIs(t, err, ErrNotFound)

			// Sizes around the multi-row INSERT batch boundary.
			for _, n := range []int{0, 1, insertBatchRows - 1, insertBatchRows, insertBatchRows + 1,
				2 * insertBatchRows, 2*insertBatchRows + 7} {
				t.Run(fmt.Sprintf("%d packages", n), func(t *testing.T) {
					inv := testInventory(n)
					require.NoError(t, s.SaveInventory(ctx, "node-1", inv))

					got, err := s.GetInventory(ctx, "node-1")
					require.NoError(t, err)
					assert.Equal(t, inv, got)

					hash, err := got.ComputeHash()
					require.NoError(t, err)
					assert.Equal(t, got.Hash, hash, "what comes back still hashes to its hash")

					host, err := s.GetHost(ctx, "node-1")
					require.NoError(t, err)
					assert.Equal(t, inv.Hash, host.InventoryHash)

					assert.Equal(t, n, countRows(t, s, "packages"), "old rows are replaced, not kept")
					assert.Equal(t, len(inv.Upgradable), countRows(t, s, "upgradable"))
					assert.Equal(t, 1, countRows(t, s, "inventories"))
				})
			}
		})
	}
}

// TestInventoryHashSurvives covers the forms of an inventory whose lists
// cannot be told apart in the tables: whatever was accepted comes back
// hashing to its own Hash.
func TestInventoryHashSurvives(t *testing.T) {
	ctx := context.Background()

	seal := func(inv *protocol.Inventory) *protocol.Inventory {
		hash, err := inv.ComputeHash()
		require.NoError(t, err)

		inv.Hash = hash

		return inv
	}

	nilUpgradable := newInventory(3)
	nilUpgradable.Upgradable = nil

	nilPackages := newInventory(0)
	nilPackages.Packages = nil
	nilPackages.Upgradable = []protocol.Upgradable{{Name: "a", Arch: "all", Current: "1", Candidate: "2"}}

	bothNil := newInventory(0)
	bothNil.Packages, bothNil.Upgradable = nil, nil

	nilPackagesEmptyUpgradable := newInventory(0)
	nilPackagesEmptyUpgradable.Packages = nil

	emptyPackagesNilUpgradable := newInventory(0)
	emptyPackagesNilUpgradable.Upgradable = nil

	nilKernels := newInventory(2)
	nilKernels.Kernels.Installed = nil

	// Two architectures of one package, and names whose order differs
	// between bytewise and case-insensitive or numeric comparison.
	ordering := newInventory(0)
	ordering.Packages = []protocol.Package{
		{Name: "Zlib", Version: "1", Arch: "all"},
		{Name: "libc6", Version: "2.41-12", Arch: "amd64"},
		{Name: "libc6", Version: "2.41-12", Arch: "i386"},
		{Name: "libc6-dev", Version: "2.41-12", Arch: "amd64"},
		{Name: "pkg-10", Version: "1", Arch: "all"},
		{Name: "pkg-9", Version: "1", Arch: "all"},
		{Name: "pkg\u00e9", Version: "1", Arch: "all"},
	}

	tests := []struct {
		name string
		inv  *protocol.Inventory
	}{
		{"both lists empty", newInventory(0)},
		{"upgradable empty", func() *protocol.Inventory {
			inv := newInventory(3)
			inv.Upgradable = []protocol.Upgradable{}

			return inv
		}()},
		{"upgradable null", nilUpgradable},
		{"packages null", nilPackages},
		{"both lists null", bothNil},
		{"packages null, upgradable empty", nilPackagesEmptyUpgradable},
		{"packages empty, upgradable null", emptyPackagesNilUpgradable},
		{"installed kernels null", nilKernels},
		{"bytewise order", ordering},
	}

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)

			addHost(t, s, "node-1", nil)

			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					inv := seal(tt.inv)
					require.NoError(t, s.SaveInventory(ctx, "node-1", inv))

					got, err := s.GetInventory(ctx, "node-1")
					require.NoError(t, err)
					assert.Equal(t, inv, got, "null and empty lists are kept apart")

					hash, err := got.ComputeHash()
					require.NoError(t, err)
					assert.Equal(t, got.Hash, hash)
				})
			}
		})
	}

	t.Run("rows changed behind the store", func(t *testing.T) {
		s := newFileStore(t)

		addHost(t, s, "node-1", nil)
		require.NoError(t, s.SaveInventory(ctx, "node-1", testInventory(3)))

		_, err := rawDB(t, s).Exec(`DELETE FROM packages`)
		require.NoError(t, err)

		got, err := s.GetInventory(ctx, "node-1")
		require.NoError(t, err)
		assert.Equal(t, []protocol.Package{}, got.Packages, "no form matches: empty, not nil")
	})
}

func TestInventoryContent(t *testing.T) {
	ctx := context.Background()
	s := newFileStore(t)

	addHost(t, s, "node-1", nil)
	addHost(t, s, "node-2", nil)

	inv := sealInventory(t, &protocol.Inventory{
		CollectedAt: time.Date(2026, 3, 1, 8, 30, 15, 123_000_000, time.UTC),
		OS:          protocol.OSInfo{ID: "debian", VersionID: "13", PrettyName: `Debian "13" 'trixie'`, Arch: "arm64"},
		Kernels:     protocol.KernelInfo{Running: "6.12.1", Newest: "6.12.1", Installed: []string{"6.12.1"}},
		Packages: []protocol.Package{
			{Name: "zlib1g", Version: "1:1.3.dfsg+really1.3.1-1", Arch: "arm64"},
			{Name: "libc6", Version: "2.41-12", Arch: "armhf"},
			{Name: "libc6", Version: "2.41-12", Arch: "arm64"},
			{Name: "Odd_Name%", Version: "1~rc1", Arch: "all"},
		},
		Upgradable: []protocol.Upgradable{
			{Name: "zlib1g", Arch: "arm64", Current: "1", Candidate: "2", Origin: "", Security: false, Blocked: true},
			{Name: "libc6", Arch: "arm64", Current: "2.41-12", Candidate: "2.41-13",
				Origin: "Debian-Security:13/stable-security", Security: true, Blocked: false},
		},
	})

	require.NoError(t, s.SaveInventory(ctx, "node-1", inv))
	require.NoError(t, s.SaveInventory(ctx, "node-2", testInventory(3)))

	got, err := s.GetInventory(ctx, "node-1")
	require.NoError(t, err)
	assert.Equal(t, inv, got, "multi-arch packages, flags, sub-second time and quoting survive")

	other, err := s.GetInventory(ctx, "node-2")
	require.NoError(t, err)
	assert.Equal(t, testInventory(3), other, "inventories of different hosts do not mix")
}

func TestSaveInventoryRejects(t *testing.T) {
	ctx := context.Background()
	s := newFileStore(t)

	addHost(t, s, "node-1", nil)

	good := testInventory(5)
	require.NoError(t, s.SaveInventory(ctx, "node-1", good))

	tampered := testInventory(5)
	tampered.Packages[0].Version = "6.6.6"

	unhashed := newInventory(5)

	// reseal sets the hash without sorting, unlike sealInventory.
	reseal := func(inv *protocol.Inventory) *protocol.Inventory {
		hash, err := inv.ComputeHash()
		require.NoError(t, err)

		inv.Hash = hash

		return inv
	}

	duplicate := newInventory(300)
	duplicate.Packages = append(duplicate.Packages, duplicate.Packages[299])
	reseal(duplicate)

	duplicateUpgradable := newInventory(25)
	duplicateUpgradable.Upgradable = append(duplicateUpgradable.Upgradable,
		duplicateUpgradable.Upgradable[len(duplicateUpgradable.Upgradable)-1])
	reseal(duplicateUpgradable)

	unsorted := newInventory(5)
	unsorted.Packages[1], unsorted.Packages[3] = unsorted.Packages[3], unsorted.Packages[1]
	reseal(unsorted)

	unsortedUpgradable := newInventory(25)
	unsortedUpgradable.Upgradable[0], unsortedUpgradable.Upgradable[2] =
		unsortedUpgradable.Upgradable[2], unsortedUpgradable.Upgradable[0]
	reseal(unsortedUpgradable)

	// Same name, architectures in the wrong order.
	unsortedArch := newInventory(0)
	unsortedArch.Packages = []protocol.Package{
		{Name: "libc6", Version: "2.41-12", Arch: "i386"},
		{Name: "libc6", Version: "2.41-12", Arch: "amd64"},
	}
	reseal(unsortedArch)

	tests := []struct {
		name    string
		host    string
		inv     *protocol.Inventory
		wantErr error
	}{
		{"nil inventory", "node-1", nil, nil},
		{"content changed after hashing", "node-1", tampered, ErrInventoryHash},
		{"no hash", "node-1", unhashed, ErrInventoryHash},
		{"unknown host", "nope", testInventory(2), ErrNotFound},
		{"same package twice", "node-1", duplicate, ErrInventoryInvalid},
		{"same upgradable twice", "node-1", duplicateUpgradable, ErrInventoryInvalid},
		{"packages not sorted", "node-1", unsorted, ErrInventoryInvalid},
		{"upgradable not sorted", "node-1", unsortedUpgradable, ErrInventoryInvalid},
		{"architectures not sorted", "node-1", unsortedArch, ErrInventoryInvalid},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := s.SaveInventory(ctx, tt.host, tt.inv)
			require.Error(t, err)

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
			}

			// A bad payload is told apart from a storage failure.
			assert.NotContains(t, err.Error(), "constraint")

			// Nothing of the rejected inventory was stored, in any table.
			got, err := s.GetInventory(ctx, "node-1")
			require.NoError(t, err)
			assert.Equal(t, good, got)

			host, err := s.GetHost(ctx, "node-1")
			require.NoError(t, err)
			assert.Equal(t, good.Hash, host.InventoryHash)

			assert.Equal(t, 1, countRows(t, s, "inventories"))
			assert.Equal(t, 5, countRows(t, s, "packages"))
		})
	}
}

// searchFixture stores inventories for hosts node-00 .. node-29, each with
// perHost generic packages plus a few named ones, and returns the number of
// package rows.
func searchFixture(t *testing.T, s Store, hosts, perHost int) int {
	t.Helper()

	ctx := context.Background()
	rows := 0

	for h := range hosts {
		name := fmt.Sprintf("node-%02d", h)
		addHost(t, s, name, nil)

		inv := newInventory(perHost)

		// libc6: two versions across the fleet, and on node-00 also a
		// second architecture of the same version.
		libc := "2.41-12"
		if h >= 20 {
			libc = "2.41-13"
		}

		inv.Packages = append(inv.Packages, protocol.Package{Name: "libc6", Version: libc, Arch: "amd64"})
		if h == 0 {
			inv.Packages = append(inv.Packages, protocol.Package{Name: "libc6", Version: libc, Arch: "i386"})
		}

		if h%2 == 0 {
			inv.Packages = append(inv.Packages,
				protocol.Package{Name: "libc6-dev", Version: libc, Arch: "amd64"})
		}

		if h < 3 {
			inv.Packages = append(inv.Packages,
				protocol.Package{Name: "glibc6-compat", Version: "1", Arch: "all"},
				protocol.Package{Name: "lib_x", Version: "1", Arch: "all"},
				protocol.Package{Name: "libax", Version: "1", Arch: "all"},
				protocol.Package{Name: "odd%name", Version: "1", Arch: "all"},
				protocol.Package{Name: `back\slash`, Version: "1", Arch: "all"},
			)
		}

		sealInventory(t, inv)
		require.NoError(t, s.SaveInventory(ctx, name, inv))

		rows += len(inv.Packages)
	}

	return rows
}

func hostRange(from, to int) []string {
	hosts := make([]string, 0, to-from)
	for h := from; h < to; h++ {
		hosts = append(hosts, fmt.Sprintf("node-%02d", h))
	}

	return hosts
}

func TestSearchPackages(t *testing.T) {
	const (
		hosts   = 30
		perHost = 150
	)

	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)

			rows := searchFixture(t, s, hosts, perHost)
			require.Equal(t, rows, countRows(t, s, "packages"))

			firstThree := []PackageVersion{{Version: "1", Hosts: hostRange(0, 3)}}
			libc6 := PackageMatch{Name: "libc6", Versions: []PackageVersion{
				{Version: "2.41-12", Hosts: hostRange(0, 20)},
				{Version: "2.41-13", Hosts: hostRange(20, 30)},
			}}
			libc6dev := PackageMatch{Name: "libc6-dev", Versions: []PackageVersion{
				{Version: "2.41-12", Hosts: []string{
					"node-00", "node-02", "node-04", "node-06", "node-08",
					"node-10", "node-12", "node-14", "node-16", "node-18",
				}},
				{Version: "2.41-13", Hosts: []string{"node-20", "node-22", "node-24", "node-26", "node-28"}},
			}}
			compat := PackageMatch{Name: "glibc6-compat", Versions: firstThree}

			tests := []struct {
				name  string
				query string
				limit int
				want  []PackageMatch
			}{
				{
					name:  "exact first, then prefix, then substring",
					query: "libc6",
					want:  []PackageMatch{libc6, libc6dev, compat},
				},
				{
					name:  "case-insensitive",
					query: "LibC6",
					want:  []PackageMatch{libc6, libc6dev, compat},
				},
				{
					name:  "surrounding space is ignored",
					query: "  libc6-dev\t",
					want:  []PackageMatch{libc6dev},
				},
				{
					name:  "limit counts names, not rows",
					query: "libc6",
					limit: 2,
					want:  []PackageMatch{libc6, libc6dev},
				},
				{
					name:  "limit one keeps the exact match",
					query: "libc6",
					limit: 1,
					want:  []PackageMatch{libc6},
				},
				{
					name:  "substring only",
					query: "c6-co",
					want:  []PackageMatch{compat},
				},
				{
					name:  "underscore is literal",
					query: "lib_x",
					want:  []PackageMatch{{Name: "lib_x", Versions: firstThree}},
				},
				{
					name:  "percent is literal",
					query: "%",
					want:  []PackageMatch{{Name: "odd%name", Versions: firstThree}},
				},
				{
					name:  "backslash is literal",
					query: `\`,
					want:  []PackageMatch{{Name: `back\slash`, Versions: firstThree}},
				},
				{name: "no match", query: "does-not-exist", want: []PackageMatch{}},
				{name: "blank query", query: "   ", want: []PackageMatch{}},
				{name: "quote is harmless", query: `' OR 1=1 --`, want: []PackageMatch{}},
			}

			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					got, err := s.SearchPackages(ctx, tt.query, tt.limit)
					require.NoError(t, err)
					assert.Equal(t, tt.want, got)
				})
			}

			t.Run("limits", func(t *testing.T) {
				// "pkg-" matches perHost names, each on every host.
				got, err := s.SearchPackages(ctx, "pkg-", 0)
				require.NoError(t, err)
				require.Len(t, got, DefaultPackageSearchLimit)
				assert.Equal(t, "pkg-0000", got[0].Name)
				assert.Equal(t, fmt.Sprintf("pkg-%04d", DefaultPackageSearchLimit-1), got[len(got)-1].Name)
				assert.Equal(t, []PackageVersion{{Version: "1.0-1", Hosts: hostRange(0, hosts)}}, got[0].Versions)

				got, err = s.SearchPackages(ctx, "pkg-", -5)
				require.NoError(t, err)
				assert.Len(t, got, DefaultPackageSearchLimit)

				got, err = s.SearchPackages(ctx, "pkg-", MaxPackageSearchLimit+1000)
				require.NoError(t, err)
				assert.Len(t, got, perHost, "all names, below the cap")

				got, err = s.SearchPackages(ctx, "pkg-00", 7)
				require.NoError(t, err)
				assert.Len(t, got, 7)
			})

			t.Run("deleted hosts drop out", func(t *testing.T) {
				require.NoError(t, s.DeleteHost(ctx, "node-01"))

				got, err := s.SearchPackages(ctx, "lib_x", 0)
				require.NoError(t, err)
				assert.Equal(t, []PackageMatch{{Name: "lib_x", Versions: []PackageVersion{
					{Version: "1", Hosts: []string{"node-00", "node-02"}},
				}}}, got)
			})
		})
	}
}

// TestSearchPackagesAtFleetSize checks search on a database the size of a
// real devnet: 30 hosts with a full Debian package list each.
func TestSearchPackagesAtFleetSize(t *testing.T) {
	const (
		hosts   = 30
		perHost = 1500
	)

	ctx := context.Background()
	s := newFileStore(t)

	rows := searchFixture(t, s, hosts, perHost)
	require.Equal(t, rows, countRows(t, s, "packages"))
	require.Greater(t, rows, 45_000)

	queries := []struct {
		query     string
		limit     int
		wantNames int
		wantFirst string
	}{
		{"libc6", 0, 3, "libc6"},
		{"pkg-0", 0, DefaultPackageSearchLimit, "pkg-0000"},
		{"pkg-", MaxPackageSearchLimit, MaxPackageSearchLimit, "pkg-0000"},
		{"-1499", 0, 1, "pkg-1499"},
		{"g", 20, 20, "glibc6-compat"},
		{"zzz", 0, 0, ""},
	}

	for _, q := range queries {
		t.Run(q.query, func(t *testing.T) {
			start := time.Now()

			got, err := s.SearchPackages(ctx, q.query, q.limit)
			require.NoError(t, err)

			elapsed := time.Since(start)

			require.Len(t, got, q.wantNames)

			if q.wantNames > 0 {
				assert.Equal(t, q.wantFirst, got[0].Name)
			}

			for _, match := range got {
				seen := 0
				for _, version := range match.Versions {
					assert.IsIncreasing(t, version.Hosts)
					seen += len(version.Hosts)
				}

				assert.NotZero(t, seen, "%s has no hosts", match.Name)
				assert.LessOrEqual(t, seen, hosts)
			}

			// Interactive search: generous enough for the race detector on
			// a loaded machine, tight enough to catch a quadratic plan.
			assert.Less(t, elapsed, 3*time.Second, "search took %s", elapsed)
			t.Logf("%q: %d names in %s", q.query, len(got), elapsed)
		})
	}
}
