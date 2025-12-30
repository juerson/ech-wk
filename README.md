## ech-worker

这是一个CF WebSocket隧道代理，包括ech workers服务端和ech client客户端。

##### 其它分支有 Durable Object 版本

```
# 拉取分支代码(选择其中一个)
git clone -b do-1 https://github.com/juerson/ech-wk.git
git clone -b do-2 https://github.com/juerson/ech-wk.git
cd ech-wk

# (已安装的忽略)全局安装 Wrangler
npm install -g wrangler

# 在当前项目安装依赖
npm install

# 连接你的 Cloudflare 账号
npx wrangler login

# (可选)本地预览与测试
npx wrangler dev

# (可选)修改 wrangler.jsonc 里面name字段的名字

# 部署到 Cloudflare
npx wrangler deploy
```

