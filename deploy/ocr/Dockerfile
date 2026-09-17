# syntax=docker/dockerfile:1
#
# 构建 ocrd（文档解析服务）单文件可执行 —— **glibc 2.17 基线版**
#
# ============================ 基线为什么是 2.17 ============================
# 2026-09-17 客户离线部署现场（CentOS 7 系，glibc 2.17 / systemd 219）：
#   skillforge-ocr 反复启动退出，8093 一直 connection refused，journal 里只有一行：
#     [PYI-16:ERROR] Failed to load Python shared library
#       '/opt/skillforge/run/ocr-tmp/_MEI00247ee9m89rGw/libpython3.11.so.1.0':
#       /lib64/libc.so.6: version `GLIBC_2.28' not found
#   —— 包里的 libpython 是**按 glibc 2.28 基线**编译的，在 2.17 的机器上根本加载不了。
#   这一类的坏处是「看起来像文件丢了」：客户会去找文件、去重装、去删缓存，全都修不好，
#   因为文件活得好好的，是它的最低 glibc 要求比目标机高。
#
# 结论：构建基线下探到**我们能支持的最老系统**（glibc 2.17 = RHEL/CentOS 7 系）。
# glibc 是向后兼容的，所以一个 2.17 产物在 2.28/2.35/2.39 上照样跑 ——
# 一份产物覆盖全部分支，不需要在离线包里放两个 ocrd，也就不需要在安装期猜目标机是谁。
# 验证方式也随之改对：verify-baseline 阶段就在 glibc 2.17 镜像里真跑一轮 OCR
# （而不是在更新的镜像里跑 —— 那等于没验证基线，本项目栽过一次：GLIBC_2.38 那次）。
#
# ============================ 三个必须讲清的坑 ============================
# 坑 1（解释器必须是 shared 的，而且必须自带 2.17 基线）：
#   PyInstaller 需要能动态加载 libpython。
#     * manylinux2014 自带的 /opt/python/cp3x 是 pypa 编的 **--enable-shared=no**，
#       PyInstaller 直接报 "Python was built without a shared library"；
#     * CentOS 7 的 base 源里没有 python3.11（SCL 才有，且要额外源）。
#   所以解释器用 **python-build-standalone 的便携 CPython 3.11**：
#   Py_ENABLE_SHARED=1，且它自己就是按 glibc 2.17 构建的 —— 正好是我们要的基线。
#   构建时它的 sha256 必须校验（与 fetch-python-runtime.sh 从 GitHub API 取到的摘要一致）。
#
# 坑 2（依赖钉版才是基线的真正决定因素）：PyInstaller 只负责把库打进包，
#   **glibc 要求由 wheel 自己决定**。所以：
#     * onnxruntime ≥1.17 只发 manylinux_2_27/2_28 wheel（要 glibc ≥2.27）→ 钉 1.16.3
#     * PyMuPDF ≥1.28 只发 manylinux_2_28 wheel → 钉 1.25.2
#     * onnxruntime 1.16 不支持 numpy 2 → 钉 numpy 1.26.4
#     * opencv-python-headless 4.10.0.84 有 manylinux2014 wheel → 可直接用
#   ⚠️ 以后想升任何一条，**先查 wheel 的 manylinux 标签**：标签一升，基线就悄悄抬高了，
#      而开发机（glibc 2.35）上完全看不出来 —— 只有客户的老机器会炸。
#
# 坑 3（GUI 依赖）：必须 opencv-python-headless。rapidocr 会拉来非 headless 的 opencv，
#   两个包往同一个 cv2 目录铺文件，谁后装谁赢 → 必须 uninstall 再 force-reinstall headless。
#   带 libGL/libX11 的产物在开发机上一切正常、在客户裸机上 cv2 加载失败且**静默失效**。
#   verify-bare 阶段在 almalinux:8（无 GUI 库的最小镜像）里真跑一轮，把这条钉死。
#
# 目标（stage）：
#   build           → /build/dist/ocrd + /build/baseline.txt
#   verify-baseline → 在 glibc 2.17（构建基线本身）里真跑 OCR，不过就构建失败
#   verify-bare     → 在无 GUI 库的最小镜像里再跑一轮（自包含性）
#   artifact        → 只含 /ocrd 与 /ocrd.baseline，便于导出
ARG MANYLINUX_IMAGE=quay.io/pypa/manylinux2014_x86_64
FROM ${MANYLINUX_IMAGE} AS build

ENV PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PIP_NO_CACHE_DIR=1 \
    PYTHONDONTWRITEBYTECODE=1

ARG PYRT_URL
ARG PYRT_SHA256
# 便携 CPython：先校验摘要再解包。摘要对不上直接失败 —— 一个被截断/替换的解释器
# 打进包，客户装完表现为「AI 跑不了代码 / 解析服务起不来」，而原因从任何输出里都看不出来。
RUN set -eux; \
    [ -n "$PYRT_URL" ] || { echo "缺 --build-arg PYRT_URL（便携 CPython 3.11 下载地址）" >&2; exit 1; }; \
    [ -n "$PYRT_SHA256" ] || { echo "缺 --build-arg PYRT_SHA256（必须校验，不许裸下载）" >&2; exit 1; }; \
    curl -fsSL -o /tmp/pyrt.tar.gz "$PYRT_URL"; \
    echo "$PYRT_SHA256  /tmp/pyrt.tar.gz" | sha256sum -c -; \
    mkdir -p /opt/py; \
    tar -xzf /tmp/pyrt.tar.gz -C /opt/py; \
    rm -f /tmp/pyrt.tar.gz; \
    test -x /opt/py/python/bin/python3.11 || { echo "解包后没有 /opt/py/python/bin/python3.11（便携运行时的目录结构变了？）" >&2; ls -l /opt/py >&2; exit 1; }; \
    /opt/py/python/bin/python3.11 -V; \
    /opt/py/python/bin/python3.11 -c 'import sysconfig; assert sysconfig.get_config_var("Py_ENABLE_SHARED")==1, "解释器不是 shared 构建，PyInstaller 用不了（见坑 1）"; print("Py_ENABLE_SHARED=1 ok")'

ENV PY=/opt/py/python/bin/python3.11 \
    PYTHONNOUSERSITE=1

# 钉版（见坑 2）。变量名就是给「升级前先查标签」用的：改这里的值必须同步跑一次
# build-ocr.sh（它会 verify-baseline 真跑，基线一破就红）。
ARG RAPIDOCR=rapidocr-onnxruntime==1.4.4
ARG PYMUPDF=PyMuPDF==1.25.2
ARG NUMPY=numpy==1.26.4
ARG ORT=onnxruntime==1.16.3
ARG OPENCV=opencv-python-headless==4.10.0.84
ARG PILLOW=Pillow==10.4.0
ARG SHAPELY=shapely==2.0.6
ARG PYCLIPPER=pyclipper==1.3.0.post6
ARG PYI=pyinstaller==6.22.2

RUN $PY -m pip install -q -U pip setuptools wheel \
 && $PY -m pip install -q "$RAPIDOCR" "$PYMUPDF" "$NUMPY" "$ORT" "$PILLOW" "$SHAPELY" "$PYCLIPPER" "$PYI" \
 && $PY -m pip uninstall -y -q opencv-python opencv-contrib-python \
 && $PY -m pip install -q --force-reinstall --no-deps "$OPENCV" \
 && $PY -c "import cv2, sys, sysconfig, onnxruntime, numpy, fitz, rapidocr_onnxruntime, shapely, pyclipper; print('cv2', cv2.__version__, '| numpy', numpy.__version__, '| ort', onnxruntime.__version__, '| fitz', fitz.version[0], '| py', sys.version.split()[0], '| shared', sysconfig.get_config_var('Py_ENABLE_SHARED'))"

WORKDIR /build
COPY ocrd.py make_fixture.py ./

# baseline.txt —— 产物的「自我说明书」，两个用途：
#   ① ocrd 起得来时：/health 与 --preflight 一起报出来（运维 curl 一下就知道跑的是哪个基线）；
#   ② ocrd 起不来时（加载器就把它挡住）：安装脚本 / 体检脚本读旁挂的 bin/ocrd.baseline，
#      仍然能直接告诉客户「这个包要 glibc ≥2.17，本机 2.17 → 满足 / 不满足」。
# 只把基线写进 Dockerfile 注释是不够的 —— 注释不会跟着离线包走到客户手里。
RUN $PY -c 'import sys; open("/build/baseline.txt","w").write("glibc=2.17\npython=%s\n" % sys.version.split()[0])' \
 && cat /build/baseline.txt

# --add-data 把整个 rapidocr_onnxruntime 包（含 models/*.onnx）塞进包内：
# 目标机不需要 Python、不需要模型文件、不需要联网下模型。
RUN RC="$($PY -c 'import rapidocr_onnxruntime,os;print(os.path.dirname(rapidocr_onnxruntime.__file__))')" \
 && echo "rapidocr at $RC" \
 && $PY -m PyInstaller -F -n ocrd \
      --add-data "$RC:rapidocr_onnxruntime" \
      --add-data "/build/baseline.txt:." \
      --collect-submodules rapidocr_onnxruntime \
      --exclude-module tkinter \
      --exclude-module matplotlib \
      --exclude-module IPython \
      --exclude-module pytest \
      ocrd.py \
 && ls -la dist/ \
 && mkdir -p /fixture \
 && $PY make_fixture.py /fixture/scan.pdf

# ---- verify-baseline：在**构建基线本身**（manylinux2014 = glibc 2.17）里真跑一轮 ----
# 这一层是「基线声明」的唯一证据：产物能在 glibc 2.17 上跑起来，就能在 glibc ≥2.17 的
# 任何发行版上跑起来（glibc 向后兼容）。在更新的镜像里验证等于没验证 —— 那正是客户
# 现场炸掉而我们本地全绿的原因。
# EXPECT 可覆盖，专为**自证**留的口子：build-ocr.sh --selfcheck 会把关键词换成
# 一个绝不可能出现在样张里的词，此时这一步**必须失败** —— 否则说明"通过"只是
# 因为服务起来了，而不是因为 OCR 真读出了字（那种绿没有意义）。
FROM ${MANYLINUX_IMAGE} AS verify-baseline
ARG EXPECT=
ENV EXPECT=${EXPECT}
COPY --from=build /build/dist/ocrd /usr/local/bin/ocrd
COPY --from=build /build/baseline.txt /ocrd.baseline
COPY --from=build /fixture/scan.pdf /fixture/scan.pdf
COPY verify_ocrd.sh /verify_ocrd.sh
RUN chmod +x /verify_ocrd.sh && /verify_ocrd.sh

# ---- verify-bare：无 GUI 库的最小镜像（almalinux:8）里再跑一轮 ----
# 这条盯的不是基线，是**自包含性**：产物不许依赖 libGL/libX11 这类只在开发机/构建镜像上
# 才有的库。装 curl 仅为测试用（curl 不引入 libGL/X11，不影响结论）。
FROM almalinux:8 AS verify-bare
ARG EXPECT=
ENV EXPECT=${EXPECT}
RUN dnf install -y -q curl && dnf clean all
COPY --from=build /build/dist/ocrd /usr/local/bin/ocrd
COPY --from=build /fixture/scan.pdf /fixture/scan.pdf
COPY verify_ocrd.sh /verify_ocrd.sh
RUN chmod +x /verify_ocrd.sh && /verify_ocrd.sh

# ---- artifact：只留两个文件，便于导出 ----
FROM scratch AS artifact
COPY --from=build /build/dist/ocrd /ocrd
COPY --from=build /build/baseline.txt /ocrd.baseline
# 必须有 CMD：scratch 段没有 CMD/ENTRYPOINT 时 `docker create` 会直接
# 报 "no command specified"，导出步骤连容器都建不起来（踩过）。
CMD ["/ocrd"]
