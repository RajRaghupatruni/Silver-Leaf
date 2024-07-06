# Silver-Leaf
Adaptive Auto Scaling with AI for Optimization (Cost, Resources etc)
### Project: OptiScale

**Description:**
OptiScale is an AI-driven adaptive auto-scaling system designed to optimize resource usage and minimize costs for cloud-based applications. This project leverages machine learning models to predict future workloads and dynamically adjust the scaling of resources. It integrates with Prometheus for real-time metrics collection, uses Flask to serve the prediction model, and provides a user-friendly React-based UI for monitoring and customization.

### Detailed Step-by-Step Implementation

#### 1. **Project Setup and Initial Configuration**

1. **Set Up Version Control:**
   - Create a repository on GitHub and clone it to your local machine.

2. **Environment Setup:**
   - Install Homebrew:
     ```sh
     /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"
     ```
   - Install Minikube:
     ```sh
     brew install minikube
     ```
   - Install Kubernetes CLI (kubectl):
     ```sh
     brew install kubectl
     ```
   - Install Python and virtual environment:
     ```sh
     brew install python
     python3 -m venv env
     source env/bin/activate
     ```
   - Install necessary Python libraries:
     ```sh
     pip install tensorflow scikit-learn pandas numpy flask joblib requests
     ```

#### 2. **Set Up Minikube**

1. **Start Minikube:**
   ```sh
   minikube start
   ```

2. **Verify Minikube Installation:**
   ```sh
   kubectl get nodes
   ```

#### 3. **Set Up Prometheus and Grafana for Monitoring**

1. **Install Helm (Kubernetes package manager):**
   ```sh
   brew install helm
   ```

2. **Add Prometheus and Grafana Helm Charts:**
   ```sh
   helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
   helm repo add grafana https://grafana.github.io/helm-charts
   helm repo update
   ```

3. **Install Prometheus:**
   ```sh
   helm install prometheus prometheus-community/prometheus
   ```

4. **Install Grafana:**
   ```sh
   helm install grafana grafana/grafana
   ```

5. **Access Grafana:**
   ```sh
   kubectl get pods --namespace default -l "app.kubernetes.io/name=grafana,app.kubernetes.io/instance=grafana"
   kubectl port-forward <grafana-pod-name> 3000:3000
   ```
   - Open a browser and go to `http://localhost:3000`
   - Default login is `admin`/`prom-operator`

#### 4. **Data Collection and Integration**

1. **Configure Prometheus to Scrape Metrics:**
   - Create a `prometheus.yaml` configuration file:
     ```yaml
     global:
       scrape_interval: 15s

     scrape_configs:
       - job_name: 'kubernetes'
         kubernetes_sd_configs:
           - role: node
         relabel_configs:
           - source_labels: [__address__]
             action: replace
             regex: (.*)
             replacement: ${1}:9100
             target_label: __address__
     ```

2. **Deploy Node Exporter for Collecting Node Metrics:**
   - Create a Node Exporter DaemonSet to collect node-level metrics:
     ```yaml
     apiVersion: apps/v1
     kind: DaemonSet
     metadata:
       labels:
         k8s-app: node-exporter
       name: node-exporter
       namespace: kube-system
     spec:
       selector:
         matchLabels:
           k8s-app: node-exporter
       template:
         metadata:
           labels:
             k8s-app: node-exporter
         spec:
           containers:
           - args:
             - --path.procfs=/host/proc
             - --path.sysfs=/host/sys
             - --collector.filesystem.ignored-mount-points
             - "^/(sys|proc|dev|host|etc)($|/)"
             image: prom/node-exporter:v1.0.1
             name: node-exporter
             ports:
             - containerPort: 9100
               hostPort: 9100
               name: metrics
             resources:
               requests:
                 memory: 30Mi
                 cpu: 100m
             securityContext:
               readOnlyRootFilesystem: true
               runAsNonRoot: true
               runAsUser: 65534
             volumeMounts:
             - mountPath: /host/proc
               name: proc
               readOnly: true
             - mountPath: /host/sys
               name: sys
               readOnly: true
           hostNetwork: true
           volumes:
           - hostPath:
               path: /proc
             name: proc
           - hostPath:
               path: /sys
             name: sys
     ```

3. **Apply the DaemonSet Configuration:**

   ```sh
   kubectl apply -f node-exporter-daemonset.yaml
   ```

4. **Update ConfigMap with Prometheus Configuration:**

   ```sh
   kubectl create configmap prometheus-config --from-file=prometheus.yaml -o yaml --dry-run | kubectl apply -f -
   ```

5. **Restart Prometheus Pod to Apply New Configuration:**

   ```sh
   kubectl delete pod -l app=prometheus
   ```

6. **Verify Node Exporter and Prometheus Integration:**

   ```sh
   kubectl get pods -l k8s-app=node-exporter -n kube-system
   ```

   Access Prometheus UI:

   ```sh
   kubectl port-forward <prometheus-pod-name> 9090:9090
   ```

   Open a browser and go to `http://localhost:9090`. Navigate to `Status` > `Targets` to ensure Node Exporter targets are listed.

#### 5. **Scrape Metrics and Write to `metrics.csv`**

Create a Python script (`scrape_metrics.py`) to periodically scrape Prometheus metrics and write them to `metrics.csv`.

```python
import requests
import csv
import time
from datetime import datetime

# Prometheus server URL
PROMETHEUS_URL = 'http://localhost:9090'

# Prometheus queries for desired metrics
QUERIES = {
    'cpu_usage': 'sum(rate(node_cpu_seconds_total[1m])) by (instance)',
    'memory_usage': 'node_memory_MemTotal_bytes - node_memory_MemAvailable_bytes',
    'network_io': 'sum(rate(node_network_receive_bytes_total[1m]) + rate(node_network_transmit_bytes_total[1m])) by (instance)',
    'request_count': 'sum(rate(http_requests_total[1m])) by (instance)'
}

# CSV file to store metrics
CSV_FILE = 'metrics.csv'

def get_prometheus_metrics():
    metrics = {}
    for metric_name, query in QUERIES.items():
        response = requests.get(f'{PROMETHEUS_URL}/api/v1/query', params={'query': query})
        results = response.json().get('data', {}).get('result', [])
        for result in results:
            instance = result['metric']['instance']
            value = float(result['value'][1])
            if instance not in metrics:
                metrics[instance] = {}
            metrics[instance][metric_name] = value
    return metrics

def write_metrics_to_csv(metrics):
    with open(CSV_FILE, mode='a', newline='') as file:
        writer = csv.writer(file)
        for instance, metric_values in metrics.items():
            row = [datetime.now().isoformat(), instance]
            for metric_name in QUERIES.keys():
                row.append(metric_values.get(metric_name, 0))
            writer.writerow(row)

def main():
    # Write CSV header if file does not exist
    try:
        with open(CSV_FILE, mode='x', newline='') as file:
            writer = csv.writer(file)
            header = ['timestamp', 'instance'] + list(QUERIES.keys())
            writer.writerow(header)
    except FileExistsError:
        pass

    while True:
        metrics = get_prometheus_metrics()
        write_metrics_to_csv(metrics)
        # Wait for 5 minutes before scraping again
        time.sleep(300)

if __name__ == '__main__':
    main()
```

#### 6. **Train Machine Learning Models with Collected Data**

1. **Data Preprocessing:**

   ```python
   import pandas as pd
   import numpy as np
   from sklearn.preprocessing import StandardScaler

   # Load data
   data = pd.read_csv('metrics.csv')

   # Convert timestamp to datetime
   data['timestamp'] = pd.to_datetime(data['timestamp'])

   # Sort data by timestamp
   data = data.sort_values(by='timestamp')

   # Fill missing values with forward fill method
   data = data.fillna(method='ffill')

   # Select features and target
   X = data[['cpu_usage', 'memory_usage', 'network_io', 'request_count']].values
   y = data['request_count'].shift(-1).fillna(0).values  # Predict next request count

   # Normalize the features
   scaler = StandardScaler()
   X_scaled = scaler.fit_transform(X)

   # Save preprocessed data
   np.save('X_scaled.npy', X_scaled)
   np.save('y.npy', y)
   ```

2. **Run the Data Preprocessing Script:**

   ```sh
   python preprocess_data.py
   ```

3. **Train the

 Model:**

   Create a Python script for model training (`train_model.py`):

   ```python
   import numpy as np
   from sklearn.model_selection import train_test_split
   from sklearn.ensemble import RandomForestRegressor
   import joblib

   # Load preprocessed data
   X = np.load('X_scaled.npy')
   y = np.load('y.npy')

   # Split data into training and testing sets
   X_train, X_test, y_train, y_test = train_test_split(X, y, test_size=0.2, random_state=42)

   # Train the model
   model = RandomForestRegressor(n_estimators=100, random_state=42)
   model.fit(X_train, y_train)

   # Evaluate the model
   score = model.score(X_test, y_test)
   print(f'Model R^2 score: {score}')

   # Save the model
   joblib.dump(model, 'model.pkl')
   ```

4. **Run the Model Training Script:**

   ```sh
   python train_model.py
   ```

#### 7. **Deploy Machine Learning Model as an API**

1. **Create a Flask API:**

   Create a file named `app.py`:

   ```python
   from flask import Flask, request, jsonify
   import joblib
   import numpy as np

   app = Flask(__name__)
   model = joblib.load('model.pkl')

   @app.route('/predict', methods=['POST'])
   def predict():
       data = request.get_json()
       metrics = np.array(data['metrics']).reshape(1, -1)
       prediction = model.predict(metrics)
       return jsonify({'prediction': prediction.tolist()})

   if __name__ == '__main__':
       app.run(host='0.0.0.0', port=5000)
   ```

2. **Run the Flask API:**

   ```sh
   python app.py
   ```

#### 8. **Integrate with Prometheus and Set Up Auto-Scaling**

1. **Custom Exporter to Send Data to Model API:**

   Create a file named `custom_exporter.py`:

   ```python
   import requests
   import prometheus_client

   metrics = prometheus_client.CollectorRegistry()

   def collect_metrics():
       # Collect data from Prometheus
       response = requests.get('http://localhost:9090/api/v1/query?query=up')
       data = response.json()['data']['result']
       metrics_data = [float(result['value'][1]) for result in data]

       # Send data to model API
       prediction = requests.post('http://localhost:5000/predict', json={'metrics': metrics_data}).json()
       return prediction['prediction']

   @prometheus_client.REGISTRY.register
   def collect():
       yield prometheus_client.core.CounterMetricFamily('predicted_scaling', 'Predicted scaling value', value=collect_metrics())

   prometheus_client.start_http_server(8000)
   ```

2. **Configure Auto-Scaling Based on Predictions:**

   Create a file named `hpa.yaml`:

   ```yaml
   apiVersion: autoscaling/v1
   kind: HorizontalPodAutoscaler
   metadata:
     name: my-app-hpa
   spec:
     scaleTargetRef:
       apiVersion: apps/v1
       kind: Deployment
       name: my-app
     minReplicas: 1
     maxReplicas: 10
     metrics:
     - type: External
       external:
         metricName: predicted_scaling
         targetAverageValue: 1
   ```

   Apply the HPA configuration:

   ```sh
   kubectl apply -f hpa.yaml
   ```

#### 9. **Visualization and User Interface**

1. **Create Grafana Dashboards:**

   - Access Grafana and create dashboards to visualize Prometheus metrics and scaling predictions.
   - Example Grafana dashboard template (JSON):

     ```json
     {
       "annotations": {
         "list": [
           {
             "builtIn": 1,
             "datasource": "Prometheus",
             "enable": true,
             "hide": true,
             "iconColor": "rgba(0, 211, 255, 1)",
             "name": "Annotations & Alerts",
             "type": "dashboard"
           }
         ]
       },
       "editable": true,
       "gnetId": null,
       "graphTooltip": 0,
       "id": null,
       "links": [],
       "panels": [
         {
           "datasource": "Prometheus",
           "fieldConfig": {
             "defaults": {
               "color": {
                 "mode": "palette-classic"
               },
               "mappings": [],
               "thresholds": {
                 "mode": "absolute",
                 "steps": [
                   {
                     "color": "green",
                     "value": null
                   },
                   {
                     "color": "red",
                     "value": 80
                   }
                 ]
               }
             },
             "overrides": []
           },
           "gridPos": {
             "h": 9,
             "w": 12,
             "x": 0,
             "y": 0
           },
           "id": 1,
           "options": {
             "tooltip": {
               "mode": "single"
             }
           },
           "targets": [
             {
               "expr": "up",
               "format": "time_series",
               "intervalFactor": 1,
               "legendFormat": "{{instance}}",
               "refId": "A"
             }
           ],
           "title": "Node Status",
           "type": "timeseries"
         },
         {
           "datasource": "Prometheus",
           "fieldConfig": {
             "defaults": {
               "color": {
                 "mode": "palette-classic"
               },
               "mappings": [],
               "thresholds": {
                 "mode": "absolute",
                 "steps": [
                   {
                     "color": "green",
                     "value": null
                   },
                   {
                     "color": "red",
                     "value": 80
                   }
                 ]
               }
             },
             "overrides": []
           },
           "gridPos": {
             "h": 9,
             "w": 12,
             "x": 12,
             "y": 0
           },
           "id": 2,
           "options": {
             "tooltip": {
               "mode": "single"
             }
           },
           "targets": [
             {
               "expr": "predicted_scaling",
               "format": "time_series",
               "intervalFactor": 1,
               "legendFormat": "{{instance}}",
               "refId": "A"
             }
           ],
           "title": "Predicted Scaling",
           "type": "timeseries"
         }
       ],
       "schemaVersion": 27,
       "style": "dark",
       "tags": [],
       "templating": {
         "list": []
       },
       "time": {
         "from": "now-6h",
         "to": "now"
       },
       "timepicker": {
         "refresh_intervals": [
           "5s",
           "10s",
           "30s",
           "1m",
           "5m",
           "15m",
           "30m",
           "1h",
           "2h",
           "1d"
         ],
         "time_options": [
           "5m",
           "15m",
           "30m",
           "1h",
           "6h",
           "12h",
           "24h",
           "2d",
           "7d",
           "30d"
         ]
       },
       "timezone": "",
       "title": "Adaptive Auto-Scaling Dashboard",
       "version": 1
     }
     ```

2. **Develop a Simple React UI:**

   Create a file named `Dashboard.js`:

   ```jsx
   import React, { useState, useEffect } from 'react';
   import axios from 'axios';

   function Dashboard() {
     const [metrics, setMetrics] = useState([]);
     const [prediction, setPrediction] = useState(null);

     useEffect(() => {
       const fetchMetrics = async () => {
         const result = await axios.get('http://localhost:9090/api/v1/query?query=up');
         setMetrics(result.data.data.result);
       };

       fetchMetrics();
     }, []);

     const predictScaling = async () => {
       const response = await axios.post('http://localhost:5000/predict', { metrics });
       setPrediction(response.data.prediction);
     };

     return (
       <div>
         <h1>Adaptive Auto-Scaling Dashboard</h1>
         <button onClick={predictScaling}>Predict Scaling</button>
         {prediction && <div>Predicted Scaling: {prediction}</div>}
         <h2>Metrics</h2>
         <pre>{JSON.stringify(metrics, null, 2)}</pre>
       </div>
     );
   }

   export default Dashboard;
   ```

### Summary

OptiScale is an AI-driven adaptive auto-scaling system designed to optimize resource usage and minimize costs. The project involves setting up a local Kubernetes environment with Minikube, collecting metrics with Prometheus, training machine learning models to predict workloads, deploying a prediction API, and configuring auto-scaling based on predictions. The system also includes a Grafana dashboard for visualization and a React-based UI for real-time monitoring and customization.