snap install go --classic
apt install zmap -y
go install
go build
ulimit -n999999; ulimit -u999999; ulimit -e999999