module github.com/k0ngk0ng/wire-download

go 1.23.0

require (
	github.com/gorilla/websocket v1.5.3
	github.com/k0ngk0ng/wirectl v0.1.0
	golang.org/x/net v0.37.0
	golang.org/x/term v0.30.0
)

require golang.org/x/sys v0.31.0 // indirect

replace github.com/k0ngk0ng/wirectl => ../wirectl
