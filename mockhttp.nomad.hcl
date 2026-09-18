job "mockhttp-vip" {
  type = "service"

  group "server" {
    network {
      mode = "cni/routed-cni"
      cni {
        args = {
          containerIP = "10.0.11.45"
          community   = "65001:100"
          local_pref  = "200"
        }
      }
    }

    task "mockhttp" {
      driver = "docker"
      config {
        image = "docker.io/jaredwray/mockhttp"
      }

      resources {
        cpu    = 100
        memory = 64
      }
    }
  }
}
