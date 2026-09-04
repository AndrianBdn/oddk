package operations

import (
	"net"
	"strconv"
	"time"

	"github.com/andrianbdn/oddk/internal/util"
)

// hostPortAnswers reports whether something on this host already accepts
// connections on the bridge gateway at port. Instances bind the gateway, which
// is host-local; if the bridge does not exist yet the dial simply fails, which
// is the correct answer — nothing can be listening there.
func hostPortAnswers(port int) bool {
	addr := net.JoinHostPort(util.GatewayIP, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
