BINDIR=build
NAME="warp-ping"
FILE=wg-ping.go
GOBUILD='go build -tags with_gvisor'

mkdir $BINDIR

env GOARCH=amd64 GOOS=linux   GOAMD64=v1    $GOBUILD  -o build/$NAME-linux-amd64 $FILE
env GOARCH=arm64 GOOS=linux                 $GOBUILD  -o build/$NAME-linux-arm64 $FILE
env GOARCH=amd64 GOOS=windows GOAMD64=v1    $GOBUILD  -o build/$NAME-windows.exe $FILE

read -p "Press any key to resume ..."