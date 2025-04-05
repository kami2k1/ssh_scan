go install
go build
zmap -p 22 -B1000M -r 10010 -T5 -q |./kami 22
### xem sex ít thôi địt com mẹ 