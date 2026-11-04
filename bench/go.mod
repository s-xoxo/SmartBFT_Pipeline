module github.com/s-xoxo/SmartBFT_Pipeline/bench

go 1.27.3

require (
	github.com/hyperledger/SmartBFT v0.0.0
	google.golang.org/protobuf v1.36.12
)

require golang.org/x/sync v0.24.0 // indirect

replace github.com/hyperledger/SmartBFT => ../../SmartBFT
