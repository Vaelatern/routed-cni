# Minimal ACL policy for routed-cni's service:nomad discovery.
# Only needs to list service registrations (Address + Port).
service {
  policy = "read"
}

node {
  policy = "read"
}
