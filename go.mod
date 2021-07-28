module storj.io/gateway-mt

go 1.13

require (
	github.com/aws/aws-sdk-go-v2 v1.1.0
	github.com/gorilla/mux v1.8.0
	github.com/hashicorp/golang-lru v0.5.4
	github.com/jackc/pgconn v1.8.0
	github.com/jackc/pgx/v4 v4.10.1
	github.com/minio/minio-go/v7 v7.0.6
	github.com/spacemonkeygo/errors v0.0.0-20201030155909-2f5f890dbc62 // indirect
	github.com/spacemonkeygo/monkit/v3 v3.0.15
	github.com/spf13/cobra v1.1.3
	github.com/spf13/pflag v1.0.5
	github.com/storj/minio v0.0.0-20210715234501-c6c6a5b367d5
	github.com/stretchr/testify v1.7.0
	github.com/zeebo/errs v1.2.2
	go.uber.org/zap v1.16.0
	golang.org/x/crypto v0.0.0-20210415154028-4f45737414dc
	golang.org/x/sync v0.0.0-20210220032951-036812b2e83c
	golang.org/x/time v0.0.0-20210611083556-38a9dc6acbc6
	google.golang.org/genproto v0.0.0-20200526211855-cb27e3aa2013 // indirect
	gopkg.in/webhelp.v1 v1.0.0-20170530084242-3f30213e4c49
	storj.io/common v0.0.0-20210708125041-4882a3ae3eda
	storj.io/private v0.0.0-20210629153057-a02a38c9d0a1
	storj.io/uplink v1.5.0-rc.1.0.20210708154526-f5ca59991bd8
)
