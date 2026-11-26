# 二进制在 Debian 上交叉编译完成（make server），Unraid 只负责 load 运行。
# 需要完全在容器内构建时，可改用 golang:1.24-alpine 多阶段构建（见 README）。
FROM gcr.io/distroless/static-debian12:nonroot
COPY bin/sentinel /sentinel
USER nonroot:nonroot
EXPOSE 8110
ENTRYPOINT ["/sentinel"]
CMD ["serve"]
