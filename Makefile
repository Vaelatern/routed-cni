routed-cni: main.go *.go
	go build .

clean:
	rm -f routed-cni
