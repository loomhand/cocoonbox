package publicnet

import "testing"

func TestRefusePrivateAddresses(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:443": false, "10.203.0.1:18090": false, "169.254.169.254:80": false, "[::1]:443": false,
		"100.64.1.1:443": false, "[::ffff:192.168.1.1]:443": false, "8.8.8.8:443": true, "[2606:4700::1111]:443": true,
	} {
		if err := RefusePrivate("tcp", addr, nil); (err == nil) != ok {
			t.Errorf("%s: %v", addr, err)
		}
	}
}
