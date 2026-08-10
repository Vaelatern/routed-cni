job "mockhttp-vip" {
  type = "service"

  group "server" {
    network {
      mode = "cni/routed-cni"
      cni {
        args = {
          containerIP = "10.0.11.45"
        }
      }
      port "http" { to = 3000 }
    }

    task "mockhttp" {
      driver = "docker"
      config {
        image = "docker.io/jaredwray/mockhttp"
        ports = ["http"]
      }

      resources {
        cpu    = 100
        memory = 64
      }
    }
  }
}
