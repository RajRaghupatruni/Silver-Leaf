import requests
import csv
import time
from datetime import datetime

# Prometheus server URL
PROMETHEUS_URL = 'http://localhost:9090'

# Prometheus query to get metrics (example queries)
QUERIES = {
    'cpu_usage': 'sum(rate(container_cpu_usage_seconds_total[2m])) by (instance)',
    'memory_usage': 'sum(container_memory_usage_bytes) by (instance)',
    'network_io': 'sum(rate(container_network_receive_bytes_total[2m])) by (instance)',
    'request_count': 'sum(rate(kubelet_http_requests_total[2m])) by (instance)'
}

# CSV file to store metrics
CSV_FILE = 'metrics.csv'

def get_prometheus_metrics():
    metrics = {}
    for metric_name, query in QUERIES.items():
        response = requests.get(f'{PROMETHEUS_URL}/api/v1/query', params={'query': query})
        results = response.json()['data']['result']
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
        time.sleep(60)

if __name__ == '__main__':
    main()
