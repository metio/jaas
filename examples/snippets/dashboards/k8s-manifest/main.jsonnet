function(name="example", namespace="default", replicas="1", image="nginx:latest")
  {
    apiVersion: "apps/v1",
    kind: "Deployment",
    metadata: {
      name: name,
      namespace: namespace,
      labels: {
        "app.kubernetes.io/name": name,
      },
    },
    spec: {
      replicas: std.parseInt(replicas),
      selector: {
        matchLabels: {
          "app.kubernetes.io/name": name,
        },
      },
      template: {
        metadata: {
          labels: {
            "app.kubernetes.io/name": name,
          },
        },
        spec: {
          containers: [
            {
              name: name,
              image: image,
              ports: [{ containerPort: 8080 }],
            },
          ],
        },
      },
    },
  }
