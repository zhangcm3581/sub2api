# Sub2API 部署与运维手册（基于私有仓库 + Docker）

> 将你修改后的代码推到自己的 GitHub 仓库，服务器上拉取后用 Docker 构建并运行，无需在服务器上安装 Go/Node.js。

---

## 一、服务器准备（Ubuntu）

### 1.1 最低配置

| 项目 | 最低 | 推荐 |
|------|------|------|
| CPU | 1 核 | 2 核+ |
| 内存 | 1 GB | 2 GB+ |
| 磁盘 | 15 GB | 30 GB+（Docker 镜像占空间） |

### 1.2 安装 Docker

```bash
sudo apt update && sudo apt install -y curl git

# 一键安装 Docker
curl -fsSL https://get.docker.com | sudo sh

# 当前用户免 sudo
sudo usermod -aG docker $USER
newgrp docker

# 验证
docker --version
docker compose version
```

### 1.3 配置 GitHub SSH（拉私有仓库）

```bash
ssh-keygen -t ed25519 -C "your-email@example.com"
cat ~/.ssh/id_ed25519.pub
# → 复制公钥，添加到 GitHub → Settings → SSH and GPG keys

ssh -T git@github.com   # 测试连通
```

> 如果是公开仓库可跳过此步，直接用 HTTPS 克隆。

---

## 二、部署

整个流程只有 4 步：**拉代码 → 改配置 → 构建镜像 → 启动**。

### 2.1 拉取代码

```bash
# 替换为你的仓库地址
git clone git@github.com:your-username/sub2api.git ~/sub2api
cd ~/sub2api
```

### 2.2 准备配置

```bash
cd ~/sub2api/deploy

# 复制环境配置模板
cp .env.example .env

# 生成安全密钥并写入
cat << EOF >> .env

# === 以下为自动生成的安全密钥 ===
POSTGRES_PASSWORD=$(openssl rand -hex 16)
JWT_SECRET=$(openssl rand -hex 32)
TOTP_ENCRYPTION_KEY=$(openssl rand -hex 32)
EOF

# 编辑确认
nano .env
```

`.env` 中需要关注的配置：

```bash
# 服务端口（外部通过 http://服务器IP:8080 访问）
SERVER_PORT=8080

# 管理员账号
ADMIN_EMAIL=admin@sub2api.local
ADMIN_PASSWORD=your_admin_password   # 留空则自动生成（在日志中查看）

# 时区
TZ=Asia/Shanghai
```

### 2.3 构建自定义镜像

```bash
cd ~/sub2api

# 用项目自带的多阶段 Dockerfile 构建（前端+后端全在容器内编译）
docker build -t sub2api:custom .
```

> 首次构建约 3-8 分钟（取决于网速和机器性能），后续增量构建很快。

### 2.4 修改 compose 文件使用自定义镜像

```bash
cd ~/sub2api/deploy

# 复制本地目录版 compose 文件
cp docker-compose.local.yml docker-compose.yml

# 在 deploy/ 目录下创建数据目录（用于持久化 PG、Redis 和应用数据）
mkdir -p data postgres_data redis_data
```

修改 `docker-compose.yml`，将镜像从官方改为你刚构建的：

```bash
sed -i 's|image: weishaw/sub2api:latest|image: sub2api:custom|' docker-compose.yml
```

### 2.5 启动

```bash
cd ~/sub2api/deploy

docker compose up -d

# 查看启动日志
docker compose logs -f sub2api
```
 
看到类似输出说明启动成功：

```
sub2api  | [Server] Listening on 0.0.0.0:8080
```

### 2.6 访问

浏览器打开：

```
http://你的服务器IP:8080
```

如果管理员密码是自动生成的：

```bash
docker compose logs sub2api 2>&1 | grep -i "admin password"
```

---

## 三、代码更新与重新部署

当你在本地修改代码并推送到 GitHub 后，在服务器上执行：

```bash
cd ~/sub2api

# 1. 拉取最新代码
git pull

# 2. 重新构建镜像
docker build -t sub2api:custom .

# 3. 重启服务（自动使用新镜像）
cd deploy
docker compose up -d

# 4. 确认
docker compose logs -f sub2api
```

### 一键更新脚本

```bash
cat << 'SCRIPT' > ~/sub2api/update.sh
#!/bin/bash
set -e
cd ~/sub2api
echo ">>> 拉取最新代码..."
git pull
echo ">>> 构建镜像..."
docker build -t sub2api:custom .
echo ">>> 重启服务..."
cd deploy && docker compose up -d
sleep 3
curl -sf http://localhost:8080/health && echo ">>> 部署成功 ✓" || echo ">>> 启动异常，请查看: docker compose logs sub2api"
SCRIPT
chmod +x ~/sub2api/update.sh
```

以后更新只需：

```bash
~/sub2api/update.sh
```

---

## 四、日常运维

### 4.1 常用命令

```bash
cd ~/sub2api/deploy

# 查看状态
docker compose ps

# 实时日志
docker compose logs -f sub2api

# 最近 100 行
docker compose logs --tail=100 sub2api

# 重启应用（不动数据库和 Redis）
docker compose restart sub2api

# 重启全部
docker compose restart

# 停止全部
docker compose down
```

### 4.2 健康检查

```bash
curl -s http://localhost:8080/health
```

### 4.3 查看资源占用

```bash
docker stats
```

### 4.4 数据库操作

```bash
# 连接 PostgreSQL
docker exec -it sub2api-postgres psql -U sub2api -d sub2api

# 查看数据库大小
docker exec -it sub2api-postgres psql -U sub2api -d sub2api \
  -c "SELECT pg_size_pretty(pg_database_size('sub2api'));"

# 查看连接数
docker exec -it sub2api-postgres psql -U sub2api -d sub2api \
  -c "SELECT count(*) FROM pg_stat_activity;"
```

### 4.5 Redis 操作

```bash
docker exec -it sub2api-redis redis-cli INFO memory | grep used_memory_human
docker exec -it sub2api-redis redis-cli DBSIZE
```

---

## 五、备份与恢复

### 5.1 自动备份

```bash
cat << 'SCRIPT' | sudo tee /usr/local/bin/sub2api-backup.sh
#!/bin/bash
set -e
BACKUP_DIR="/opt/backups/sub2api"
DATE=$(date +%Y%m%d_%H%M%S)
mkdir -p "$BACKUP_DIR"

# 备份数据库
docker exec sub2api-postgres pg_dump -U sub2api -d sub2api | gzip > "$BACKUP_DIR/db_${DATE}.sql.gz"

# 备份配置
cp ~/sub2api/deploy/.env "$BACKUP_DIR/env_${DATE}.bak"

# 清理 7 天前的备份
find "$BACKUP_DIR" -name "*.gz" -mtime +7 -delete
find "$BACKUP_DIR" -name "*.bak" -mtime +7 -delete

echo "[$(date)] Backup done: db_${DATE}.sql.gz"
SCRIPT

sudo chmod +x /usr/local/bin/sub2api-backup.sh

# 每天凌晨 3 点执行
(crontab -l 2>/dev/null; echo "0 3 * * * /usr/local/bin/sub2api-backup.sh >> /var/log/sub2api-backup.log 2>&1") | crontab -
```

### 5.2 恢复数据库

```bash
gunzip -c /opt/backups/sub2api/db_20240101_030000.sql.gz | \
  docker exec -i sub2api-postgres psql -U sub2api -d sub2api
```

### 5.3 整体迁移

```bash
# 旧服务器
cd ~/sub2api/deploy
docker compose down
cd ~
tar czf sub2api-full.tar.gz sub2api/

# 传输
scp sub2api-full.tar.gz user@new-server:~/

# 新服务器（装好 Docker 后）
tar xzf sub2api-full.tar.gz
cd sub2api
docker build -t sub2api:custom .
cd deploy
docker compose up -d
```

---

## 六、故障排查

| 现象 | 排查命令 | 常见原因 |
|------|---------|---------|
| 容器启动失败 | `docker compose logs sub2api` | 数据库密码错误、端口冲突 |
| 无法访问页面 | `curl localhost:8080/health` | 防火墙未放行端口 |
| 数据库连不上 | `docker compose logs postgres` | PG 还没 ready、密码不匹配 |
| 上游请求报错 | `docker compose logs sub2api \| grep error` | 网络不通、代理配置问题 |
| 内存不足 | `docker stats` / `free -m` | 增加内存或限制容器资源 |

**放行端口（如果用了 UFW 防火墙）：**

```bash
sudo ufw allow 8080/tcp
```

**Docker 日志太大？配置轮转：**

```bash
sudo tee /etc/docker/daemon.json << 'EOF'
{
  "log-driver": "json-file",
  "log-opts": { "max-size": "50m", "max-file": "3" }
}
EOF
sudo systemctl restart docker
```

---

## 七、可选：Nginx 反向代理

如果后续想用域名或 HTTPS，加一层 Nginx：

```bash
sudo apt install -y nginx

sudo tee /etc/nginx/sites-available/sub2api << 'NGINX'
underscores_in_headers on;
server {
    listen 80;
    server_name your-domain.com;    # 或 _ 表示匹配所有
    client_max_body_size 256m;
    proxy_buffering off;
    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header Connection "";
        proxy_read_timeout 600s;
    }
    location ~* /ws {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_read_timeout 3600s;
    }
}
NGINX

sudo ln -sf /etc/nginx/sites-available/sub2api /etc/nginx/sites-enabled/
sudo rm -f /etc/nginx/sites-enabled/default
sudo nginx -t && sudo systemctl reload nginx

# 如果要 HTTPS
sudo apt install -y certbot python3-certbot-nginx
sudo certbot --nginx -d your-domain.com
```

---

## 速查卡片

```bash
# ===== 部署 =====
cd ~/sub2api && docker build -t sub2api:custom .
cd deploy && docker compose up -d

# ===== 更新 =====
~/sub2api/update.sh

# ===== 日志 =====
cd ~/sub2api/deploy && docker compose logs -f sub2api

# ===== 重启 =====
cd ~/sub2api/deploy && docker compose restart sub2api

# ===== 停止 =====
cd ~/sub2api/deploy && docker compose down

# ===== 备份 =====
sudo sub2api-backup.sh

# ===== 健康检查 =====
curl http://localhost:8080/health
```
