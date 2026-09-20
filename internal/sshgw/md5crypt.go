package sshgw

import (
	"crypto/md5"
	"crypto/subtle"
	"strings"
)

// md5crypt implements the FreeBSD/Linux "$1$" password hash the controller
// uses for the device SSH password it pushes (users.N.password). Standard
// algorithm (Poul-Henning Kamp, 1994); verified against openssl passwd -1.
func md5crypt(password, salt string) string {
	const magic = "$1$"
	if len(salt) > 8 {
		salt = salt[:8]
	}
	pw := []byte(password)
	sb := []byte(salt)

	alt := md5.New()
	alt.Write(pw)
	alt.Write(sb)
	alt.Write(pw)
	altSum := alt.Sum(nil)

	h := md5.New()
	h.Write(pw)
	h.Write([]byte(magic))
	h.Write(sb)
	for n := len(pw); n > 0; n -= 16 {
		if n > 16 {
			h.Write(altSum)
		} else {
			h.Write(altSum[:n])
		}
	}
	for n := len(pw); n > 0; n >>= 1 {
		if n&1 != 0 {
			h.Write([]byte{0})
		} else {
			h.Write(pw[:1])
		}
	}
	sum := h.Sum(nil)

	for i := 0; i < 1000; i++ {
		r := md5.New()
		if i&1 != 0 {
			r.Write(pw)
		} else {
			r.Write(sum)
		}
		if i%3 != 0 {
			r.Write(sb)
		}
		if i%7 != 0 {
			r.Write(pw)
		}
		if i&1 != 0 {
			r.Write(sum)
		} else {
			r.Write(pw)
		}
		sum = r.Sum(nil)
	}

	const itoa64 = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	var out strings.Builder
	out.WriteString(magic)
	out.WriteString(salt)
	out.WriteByte('$')
	to64 := func(v uint32, n int) {
		for ; n > 0; n-- {
			out.WriteByte(itoa64[v&0x3f])
			v >>= 6
		}
	}
	to64(uint32(sum[0])<<16|uint32(sum[6])<<8|uint32(sum[12]), 4)
	to64(uint32(sum[1])<<16|uint32(sum[7])<<8|uint32(sum[13]), 4)
	to64(uint32(sum[2])<<16|uint32(sum[8])<<8|uint32(sum[14]), 4)
	to64(uint32(sum[3])<<16|uint32(sum[9])<<8|uint32(sum[15]), 4)
	to64(uint32(sum[4])<<16|uint32(sum[10])<<8|uint32(sum[5]), 4)
	to64(uint32(sum[11]), 2)
	return out.String()
}

// verifyMD5Crypt reports whether password matches a "$1$salt$hash" string.
func verifyMD5Crypt(password, hash string) bool {
	if !strings.HasPrefix(hash, "$1$") {
		return false
	}
	parts := strings.SplitN(hash[3:], "$", 2)
	if len(parts) != 2 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(md5crypt(password, parts[0])), []byte(hash)) == 1
}
