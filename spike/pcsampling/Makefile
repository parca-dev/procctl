CUDA_HOME ?= /usr/local/cuda
NVCC      ?= $(CUDA_HOME)/bin/nvcc
ARCH      ?= native

# CUPTI headers/libs live in extras/CUPTI on older toolkits and in the main
# include/lib dirs on newer ones; search both.
INC  = -I$(CUDA_HOME)/extras/CUPTI/include -I$(CUDA_HOME)/include
LIBS = -L$(CUDA_HOME)/extras/CUPTI/lib64 -L$(CUDA_HOME)/lib64 \
       -Xlinker -rpath,$(CUDA_HOME)/extras/CUPTI/lib64 \
       -Xlinker -rpath,$(CUDA_HOME)/lib64 \
       -lcupti -lcuda

pcprobe: pcprobe.cu
	$(NVCC) -O2 -arch=$(ARCH) -lineinfo $(INC) -o $@ $< $(LIBS)

ctlthread: ctlthread.cu
	$(NVCC) -O2 -arch=$(ARCH) $(INC) -o $@ $< $(LIBS) -lpthread

clean:
	rm -f pcprobe ctlthread

.PHONY: clean
