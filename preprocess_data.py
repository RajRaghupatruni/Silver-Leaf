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
