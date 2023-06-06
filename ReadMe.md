# Go Warp Scanner

A warp scanner that scans throat a real wireguard client! 

### Run in Android in Termux 

```
pkg install unzip wget
wget https://github.com/MiSaturo/Go-Warp-Scanner/files/11646039/warp-ping-linux-arm64.zip
unzip http://warp-ping-linux-arm64.zip
chmod +x warp-ping-linux-arm64
./warp-ping-linux-arm64
```

### Set custom endpiont on windows client

```
warp-cli.exe set-custom-endpoint %endpoint%
```