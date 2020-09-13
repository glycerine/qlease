
all:
	cd client; go build -o $(GOPATH)/bin/qlease-client
	cd server; go build -o $(GOPATH)/bin/qlease-server
	cd master; go build -o $(GOPATH)/bin/qlease-master

run:
	qlease-master &
	qlease-server -port 7070 &
	qlease-server -port 7071 &
	qlease-server -port 7072 &
	qlease-client
