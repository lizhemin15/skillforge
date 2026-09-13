# syntax=docker/dockerfile:1
#
# 构建 ocrd（文档解析服务）单文件可执行 —— 为什么用容器构建而不是本机 venv：
#
# 1) **可复现**：本机 venv 会随时间漂移（pip 装过别的东西），产物不可复现；容器每次干净。
# 2) **零系统依赖可验证**：PyInstaller 会把构建环境里找到的 .so 一起打包，本机构建容易
#    「本机能跑、客户机不能跑」。构建完必须在**没有 GUI 库的最小镜像**里真跑一轮才算过关，
#    本文件的 verify 阶段就是那道门。
# 3) **双架构**：arm64 用 buildx + QEMU —— PyInstaller 不支持交叉编译，
#    只能在 arm64 容器里跑 arm64 的 python（QEMU 慢，但结果正确）。
#
# ---- 两个必须讲清楚的坑（都是实测踩出来的，不是理论）----
#
# 坑 1：GUI 依赖。用 opencv-python（非 headless）会带 libGL/libX11，而 PyInstaller
#   **不会**把它们收进包 → 有桌面的开发机上一切正常，客户机的裸 server 上 cv2 加载失败、
#   OCR 静默失效（接口仍返回 200，只是抽不出文本）。所以用 opencv-python-headless。
#   注意 rapidocr 自己依赖 opencv-python，两个包往同一个 cv2 目录铺文件，谁后装谁赢：
#   必须 uninstall 再 force-reinstall headless 才真正生效。
#
# 坑 2：glibc 基线（这个更隐蔽）。PyInstaller 不打包 libc，产物的 glibc 要求 = **构建镜像的
#   glibc 版本**。用 python:3.11-slim 构建出过要求 GLIBC_2.38 的产物 —— 在 Debian 12 /
#   Ubuntu 22.04 / RHEL 9 上都起不来（报 "version GLIBC_2.38 not found"），
#   而这在构建机上完全看不出来。所以构建镜像固定用 **manylinux_2_28**（AlmaLinux 8 系，
#   glibc 2.28）：产物在 RHEL/CentOS 8+、Debian 10+、Ubuntu 18.10+ 上都能跑。
#   verify 阶段就在 almalinux:8（glibc 2.28，正好是基线）里跑，把这条要求钉死。
#
# 坑 3：manylinux 自带的 /opt/python/* 不能用 PyInstaller（这个最阴）。pypa 编的 CPython
#   是 **--enable-shared=no**（实测 cp311-cp311 的 Py_ENABLE_SHARED=0），而 PyInstaller 6
#   在 Linux 上必须链 libpython，直接报：
#     "Python was built without a shared library, which is required by PyInstaller."
#   注意别被 /usr/lib64/libpython3.11.so.1.0 骗了：AlmaLinux 8 的 appstream python3.11
#   （/usr/bin/python3.11，3.11.13）**是** shared（Py_ENABLE_SHARED=1），跟 pypa 那个不同物。
#   而且 manylinux 镜像把 /opt/python/cp311-cp311/bin 放在 PATH 前面，`python3.11` 会解析到
#   那个坏的 → 必须**写绝对路径 /usr/bin/python3.11**，不能用 PATH 上的名字。
#   好处：appstream 的 python3.11 也是 glibc 2.28 基线，坑 2 的要求一点没破。
#
# 目标（stage）：
#   build    → 产物在 /build/dist/ocrd
#   verify   → 在基线镜像里跑真 OCR 一轮，不过就构建失败
#   artifact → 只含 /ocrd 一个文件，便于 `docker cp` / --output type=local 导出
ARG MANYLINUX_IMAGE=quay.io/pypa/manylinux_2_28_x86_64
FROM ${MANYLINUX_IMAGE} AS build

# 用发行版 python3.11（shared），不用 manylinux 的 /opt/python/*（见坑 3）。
ENV PY=/usr/bin/python3.11 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PIP_NO_CACHE_DIR=1 \
    PYTHONDONTWRITEBYTECODE=1

ARG RAPIDOCR=rapidocr-onnxruntime==1.4.4
ARG PYMUPDF=pymupdf==1.28.2
ARG OPENCV=opencv-python-headless==5.0.0.93
ARG PYI=pyinstaller==6.22.2

# binutils 提供 objdump（PyInstaller 分析 ELF 依赖时必需，缺了直接报错退出）
RUN dnf install -y -q binutils patchelf python3.11 python3.11-pip \
 && dnf clean all \
 && $PY -m pip install -q -U pip setuptools wheel \
 && $PY -m pip install -q "$RAPIDOCR" "$PYMUPDF" "$PYI" \
 && $PY -m pip uninstall -y -q opencv-python opencv-contrib-python \
 && $PY -m pip install -q --force-reinstall --no-deps "$OPENCV" \
 && $PY -c "import cv2, sysconfig, sys; print('cv2', cv2.__version__, sys.version.split()[0], 'shared', sysconfig.get_config_var('Py_ENABLE_SHARED'))"

WORKDIR /build
COPY ocrd.py make_fixture.py ./

# --add-data 把整个 rapidocr_onnxruntime 包（含 models/*.onnx）塞进包内：
# 目标机不需要 Python、不需要模型文件、不需要联网下模型。
RUN RC="$($PY -c 'import rapidocr_onnxruntime,os;print(os.path.dirname(rapidocr_onnxruntime.__file__))')" \
 && echo "rapidocr at $RC" \
 && $PY -m PyInstaller -F -n ocrd \
      --add-data "$RC:rapidocr_onnxruntime" \
      --collect-submodules rapidocr_onnxruntime \
      --exclude-module tkinter \
      --exclude-module matplotlib \
      --exclude-module IPython \
      --exclude-module pytest \
      ocrd.py \
 && ls -la dist/ \
 && mkdir -p /fixture \
 && $PY make_fixture.py /fixture/scan.pdf

# ---- verify：glibc 基线的真实镜像 + 只拷 ocrd 一个文件 ----
# 用 almalinux:8 而不是 debian:12：它的 glibc 正好是 2.28（manylinux_2_28 的基线），
# 在这一层能跑起来 = 产物对「老发行版」成立。用新镜像验证等于没验证（掩盖坑 2）。
# 装 curl 仅为测试用（curl 不引入 libGL/X11，不影响结论）。
FROM almalinux:8 AS verify
# EXPECT 可覆盖，专为**自证**留的口子：build-ocr.sh --selfcheck 会把关键词换成
# 一个绝不可能出现在样张里的词。此时这一步**必须失败** —— 否则说明"通过"只是
# 因为服务起来了，而不是因为 OCR 真读出了字（那种绿没有意义）。
ARG EXPECT=
ENV EXPECT=${EXPECT}
RUN dnf install -y -q curl && dnf clean all
COPY --from=build /build/dist/ocrd /usr/local/bin/ocrd
COPY --from=build /fixture/scan.pdf /fixture/scan.pdf
COPY verify_ocrd.sh /verify_ocrd.sh
RUN chmod +x /verify_ocrd.sh && /verify_ocrd.sh

# ---- artifact：只留一个文件，便于导出 ----
FROM scratch AS artifact
COPY --from=build /build/dist/ocrd /ocrd
