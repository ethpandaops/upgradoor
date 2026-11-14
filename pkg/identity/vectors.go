package identity

// Vector is one fixed identity test vector. The Go tests and the Ansible
// filter plugin tests both recompute every output from the inputs and compare
// byte for byte against testdata/vectors.json.
type Vector struct {
	Name string `json:"name"`

	// Inputs.
	Seed       string          `json:"seed"`
	Network    string          `json:"network"`
	Hostname   string          `json:"hostname"`
	Generation uint64          `json:"generation"`
	Validators *ValidatorRange `json:"validators,omitempty"`
	Roles      []string        `json:"roles"`

	// Outputs.
	SignerPublicKey string `json:"signerPublicKey"`
	HostKey         string `json:"hostKey"`
	HostPublicKey   string `json:"hostPublicKey"`
	Certificate     string `json:"certificate"`
	CertificateHash string `json:"certificateHash"`

	// Proof is a proof with fixed claims, signed by the host key.
	ProofClaims ProofClaims `json:"proofClaims"`
	ProofBody   string      `json:"proofBody"`
	Proof       string      `json:"proof"`
}

// Compute fills the outputs of v from its inputs.
func (v *Vector) Compute() error {
	seed := []byte(v.Seed)

	signer, err := DeriveSignerKey(seed, v.Network)
	if err != nil {
		return err
	}

	host, err := DeriveHostKey(seed, v.Network, v.Hostname, v.Generation)
	if err != nil {
		return err
	}

	cert, err := IssueCertificate(seed, CertificateSpec{
		Network:    v.Network,
		Hostname:   v.Hostname,
		Generation: v.Generation,
		Validators: v.Validators,
		Roles:      v.Roles,
	})
	if err != nil {
		return err
	}

	v.ProofClaims.BodyHash = BodyHash([]byte(v.ProofBody))

	proof, err := SignProof(host, &v.ProofClaims)
	if err != nil {
		return err
	}

	v.SignerPublicKey = EncodePublicKey(PublicKey(signer))
	v.HostKey = EncodePrivateKey(host)
	v.HostPublicKey = EncodePublicKey(PublicKey(host))
	v.Certificate = cert
	v.CertificateHash = CertificateHash(cert)
	v.Proof = proof

	return nil
}

// VectorInputs returns the inputs of the fixed test vectors, without outputs.
func VectorInputs() []Vector {
	proof := func(host string) ProofClaims {
		return ProofClaims{
			IssuedAt:  1800000000,
			ExpiresAt: 1800000060,
			ID:        "AAECAwQFBgcICQoLDA0ODw",
			Method:    "POST",
			URL:       "https://upgradoor." + host + "/api/agent/v1/sync",
		}
	}

	const (
		seedA    = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		netA     = "testnet-devnet-0"
		domainA  = netA + ".ethpandaops.io"
		roleNode = "ethereum_node"
	)

	return []Vector{
		{
			Name: "validator-host", Seed: seedA,
			Network: netA, Hostname: "lighthouse-geth-1",
			Validators:  &ValidatorRange{Start: 0, End: 100},
			Roles:       []string{roleNode},
			ProofClaims: proof(domainA),
			ProofBody:   `{"protocolVersion":1}`,
		},
		{
			Name: "bootnode-no-validators", Seed: seedA,
			Network: netA, Hostname: "bootnode-1",
			Roles:       []string{"bootnode"},
			ProofClaims: proof(domainA),
			ProofBody:   "",
		},
		{
			Name: "rotated-generation", Seed: seedA,
			Network: netA, Hostname: "lighthouse-geth-1", Generation: 3,
			Validators:  &ValidatorRange{Start: 0, End: 100},
			Roles:       []string{roleNode},
			ProofClaims: proof(domainA),
			ProofBody:   `{"protocolVersion":1}`,
		},
		{
			Name:    "roles-sorted-and-deduplicated",
			Seed:    "correct horse battery staple, but at least 32 bytes long!",
			Network: "glamsterdam-devnet-8", Hostname: "prysm-besu-1",
			Validators:  &ValidatorRange{Start: 3100, End: 3200},
			Roles:       []string{roleNode, "bootnode", roleNode, "Mev.relay_1"},
			ProofClaims: proof("glamsterdam-devnet-8.ethpandaops.io"),
			ProofBody:   "non-ASCII body ✓ with a newline\n",
		},
		{
			Name:    "no-roles",
			Seed:    "ünïcödé seed bytes are used as raw UTF-8 input ✓",
			Network: "net.with.dots", Hostname: "Host_With-Mixed.Case01",
			Generation:  18446744073709551615,
			Validators:  &ValidatorRange{Start: 4294967296, End: 4294967297},
			Roles:       nil,
			ProofClaims: proof("net.with.dots.example"),
			ProofBody:   "x",
		},
	}
}
