package sshgw

import "testing"

func TestMD5Crypt(t *testing.T) {
	// Vectors from `openssl passwd -1 -salt <salt> <password>`.
	cases := []struct{ pw, salt, want string }{
		{"password", "saltsalt", "$1$saltsalt$qjXMvbEw8oaL.CzflDtaK/"},
		{"hunter2", "kIaKtIJL", "$1$kIaKtIJL$TcfFPKavtvcgc4VtUt4ql1"},
	}
	for _, c := range cases {
		if got := md5crypt(c.pw, c.salt); got != c.want {
			t.Errorf("md5crypt(%q,%q) = %s, want %s", c.pw, c.salt, got, c.want)
		}
		if !verifyMD5Crypt(c.pw, c.want) || verifyMD5Crypt(c.pw+"x", c.want) {
			t.Errorf("verify failed for %q", c.pw)
		}
	}
}
