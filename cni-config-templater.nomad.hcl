job "cni-config-templater" {
  type = "system"

  group "templater" {
    constraint {
      attribute = "${attr.kernel.name}"
      value     = "linux"
    }

    task "render-cni-config" {
      driver = "raw_exec"

      config {
        command = "/bin/sh"
        args    = ["-c", "while true; do sleep 3600; done"]
      }

      template {
        destination = "/etc/cni/net.d/10-routed-cni.conflist"
        perms       = "0644"
        data        = <<EOH
{{- $hostIP := env "attr.unique.network.ip-address" -}}
{
  "cniVersion": "1.0.0",
  "name": "routed-cni",
  "plugins": [
    {
      "type": "routed-cni",
      "gwIP": "{{ $hostIP }}",
      "gocast": "http://127.0.0.1:{{- range nomadService "gocast" }}{{ if eq .Address $hostIP }}{{ .Port }}{{ end }}{{ end }}",
      "prefix": 32
    }
  ]
}
EOH
      }

      resources {
        cpu    = 50
        memory = 32
      }
    }
  }
}
