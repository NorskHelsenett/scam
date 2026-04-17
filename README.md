# ArgoCD deployment

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: scam
  namespace: argocd
spec:
  destination:
    namespace: nhn-scam
    server: https://kubernetes.default.svc
  project: default
  source:
    chart: helm
    repoURL: ghcr.io/norskhelsenett/scam
    targetRevision: '*'
  syncPolicy:
    automated:
      enabled: true
      prune: true
      selfHeal: true
    syncOptions:
    - CreateNamespace=true
```
