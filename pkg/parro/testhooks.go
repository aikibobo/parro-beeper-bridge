package parro

// SetEndpointsForTesting points the client at fake servers and allows plain-http media hosts.
// It returns a function that restores the real endpoints. Only tests call this.
func SetEndpointsForTesting(rest, idp string) (restore func()) {
	oldRest, oldIDP, oldInsecure := restBase, idpBase, allowInsecureHosts
	restBase, idpBase, allowInsecureHosts = rest, idp, true
	return func() { restBase, idpBase, allowInsecureHosts = oldRest, oldIDP, oldInsecure }
}
