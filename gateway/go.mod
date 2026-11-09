module github.com/JackMahfouz/e-wallet/gateway

go 1.26.10

require (
	github.com/JackMahfouz/e-wallet/libs/auth v0.0.0-00010101000000-000000000000
	github.com/alicebob/miniredis/v2 v2.40.0
	github.com/redis/go-redis/v9 v9.23.0
	google.golang.org/grpc v1.85.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
	go.uber.org/atomic v1.12.0 // indirect
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260817212433-ac3dfec99bb1 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

replace github.com/JackMahfouz/e-wallet/libs/auth => ../libs/auth
