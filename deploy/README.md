# pingatlas 部署脚本

## 备份

```bash
sudo /opt/pingatlas/deploy/backup.sh                # 完整备份（含探测明细）
sudo /opt/pingatlas/deploy/backup.sh --config-only   # 只备份配置类表（不含 pinglog 明细，体积小）
```

产物在 `/opt/pingatlas/backups/`：

```
pingatlas-backup-YYYYmmdd-HHMMSS.tar.gz          # 内含 db.dump（pg_dump -Fc）/ center.json / node.key(若有) / MANIFEST.txt
pingatlas-backup-YYYYmmdd-HHMMSS.tar.gz.sha256   # 校验和
backup.log                                 # 最近 500 行日志
```

**保留策略**：普通备份保留 `PINGATLAS_KEEP_DAYS`（默认 14）天；周日备份额外保留 `PINGATLAS_KEEP_WEEKS`（默认 8）周。
归档与目录权限分别是 `600` / `700`（里面有数据库口令与节点私钥）。

可覆盖的环境变量：`PINGATLAS_DIR`（默认 `/opt/pingatlas`）、`PINGATLAS_BACKUP_DIR`、`PINGATLAS_KEEP_DAYS`、`PINGATLAS_KEEP_WEEKS`。

### 定时任务

```bash
sudo install -m 644 /opt/pingatlas/deploy/sp2-backup.service /etc/systemd/system/
sudo install -m 644 /opt/pingatlas/deploy/sp2-backup.timer   /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now sp2-backup.timer
systemctl list-timers sp2-backup.timer        # 查看下次执行时间
sudo systemctl start sp2-backup.service       # 立刻跑一次
journalctl -u sp2-backup.service -n 50        # 看日志
```

不想用 systemd 也可以用 cron：

```cron
20 3 * * * /opt/pingatlas/deploy/backup.sh >> /var/log/sp2-backup.log 2>&1
```

## 恢复

```bash
/opt/pingatlas/deploy/restore.sh backups/pingatlas-backup-20261004-032000.tar.gz                 # 恢复到临时库并校验
/opt/pingatlas/deploy/restore.sh backups/...tar.gz --drop-scratch                          # 校验完删掉临时库
/opt/pingatlas/deploy/restore.sh backups/...tar.gz --prod --yes                            # 覆盖生产库（危险，双确认）
```

默认**绝不动生产库**：先恢复到 `sp2_restore_<月日时分>`，逐表比对行数（生产 vs 恢复）后由你决定保留或删除。

## 节点私钥

- 中心所在主机若同时跑节点（`/etc/pingatlas/node.key` 存在），备份会一并打包。
- **其它节点的私钥需要各自备份**（每台执行一次）：

```bash
sudo install -m 600 /etc/pingatlas/node.key /root/sp2-node-$(hostname).key   # 留在本机
# 或取回中心统一存放：
# scp ubuntu@<节点IP>:/tmp/node.key ./node-$(hostname).key   # 需先在节点上 sudo cp 到可读位置
```

私钥丢失不会导致监控中断（节点会用原私钥继续连），但**重新安装/换机**时需要它才能保持同一身份；
若私钥确实丢了，可在管理面板用 `node/key/reset` 让该节点重新登记身份。
