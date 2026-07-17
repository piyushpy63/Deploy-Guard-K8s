# Deploy Guard Demo Application 🧪

This is a tiny, zero-dependency demo setup to test Deploy Guard in action. You will deploy a healthy application, intentionally break it, and watch Deploy Guard automatically roll it back to a healthy state in under 1 minute.

---

### Step 1: Create Namespace and Deploy App
Create the target namespace and deploy the healthy version of the demo application:
```bash
# 1. Create the namespace
kubectl create namespace demo

# 2. Deploy the healthy app (running standard Nginx)
kubectl apply -f deployment.yaml
```

Wait until all pods are running and stable:
```bash
kubectl get pods -n demo
```

---

### Step 2: Stream Deploy Guard Logs
In a separate terminal tab, tail the Deploy Guard logs to watch evaluations:
```bash
kubectl logs -f deployment/deploy-guard -n deploy-guard
```

---

### Step 3: Intentionally Break the App
Trigger a new rollout with a broken container entrypoint (the pods will crash loop immediately upon start):
```bash
kubectl patch deployment demo-app -n demo --type='json' -p='[{"op": "add", "path": "/spec/template/spec/containers/0/command", "value": ["sh", "-c", "exit 1"]}]'
```

---

### Step 4: Watch the Automated Rollback
In your Deploy Guard logs terminal, you will see:
1. A new rollout event is detected for `demo/demo-app`.
2. Deploy Guard captures the healthy baseline of metrics.
3. Once the failing pods start crash looping, `pod_restarts` spikes.
4. Deploy Guard scores the health (drops below the safety threshold `0.6` due to restart penalty).
5. Deploy Guard triggers the rollback:
   `🚨 ROLLBACK triggered for demo/demo-app`
   `Reverting to last stable revision...`
6. Verify that the pods in the `demo` namespace have successfully rolled back and are healthy again:
   ```bash
   kubectl get pods -n demo
   ```
