# PIVX Block Explorer

Customized version of TREZOR block indexer: https://github.com/trezor/blockbook

Tested on Ubuntu 18, these instructions are for a manual or ad-hoc build.

INSTALL THESE PACKAGES
```bash
sudo apt-get update
sudo apt-get install -y build-essential software-properties-common lz4 zstd libsnappy-dev libbz2-dev libzmq3-dev golang librocksdb-dev liblz4-dev libjemalloc-dev libgflags-dev libsnappy-dev zlib1g-dev libbz2-dev libzstd-dev
```

FOR CHARTS TO WORK YOU WILL NEED TO INSTALL
```bash
sudo apt-get install -y python3-pip
pip install bitcoinrpc python-bitcoinrpc
```

CREATE THE DIRECTORIES
```bash
mkdir go
```

CREATE THE GO PATH
```
GOPATH=/home/<USER>/go   #CHANGE <USER> to your user name
```

```bash
cd go && mkdir src && cd src
git clone https://github.com/PIVX-Project/PIVX-BlockExplorer blockbook
cd blockbook
```

Edit `build/blockchaincfg.json` or `build/tnblockchaincfg.json` adding wallet info

```bash
go mod init
go mod tidy
go build
```
YOU WILL GET AN ERROR MESSAGE...THATS OK


RUN THESE SCRIPTS
```bash
./build.sh
```

SETUP NGINX and SSL certs

UNCOMMENT line for MAINNET or TESTNET
```bash
./launch.sh
```

Edit the following 2 python scripts with your RPC credentials
```bash
contrib/scripts/charts/updateCharts_blocks.py
contrib/scripts/charts/updateCharts_github.py
```
make sure you update config in `updateCharts_blocks.py` matching your pivxd credentials<br>

run these every hour in a cron (`crontab -e`)
```cron
@hourly python3 /<full/path/to>/updateCharts_blocks.py
@hourly python3 /<full/path/to>/updateCharts_github.py
```

#### Out of memory when doing initial synchronization

How to reduce memory footprint of the initial sync:

- disable rocksdb cache by parameter `-dbcache=0`, the default size is 500MB
- run blockbook with parameter `-workers=1`. This disables bulk import mode, which caches a lot of data in memory (not in rocksdb cache). It will run about twice as slowly but especially for smaller blockchains it is no problem at all.
