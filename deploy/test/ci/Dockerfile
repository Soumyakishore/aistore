FROM golang:1.15

ENV GOPATH="/go"
ENV PATH="${GOPATH}/bin:${PATH}"

RUN apt-get update -yq
RUN apt-get --no-install-recommends -y install curl git sysstat attr build-essential lsof fuse coreutils python3-pip python3-setuptools
RUN pip3 install yapf pylint awscli s3cmd

# Install `kubectl`.
RUN curl -LO https://storage.googleapis.com/kubernetes-release/release/$(curl -s https://storage.googleapis.com/kubernetes-release/release/stable.txt)/bin/linux/amd64/kubectl
RUN chmod +x ./kubectl
RUN mv ./kubectl /usr/local/bin/kubectl
RUN kubectl version --client

# Cache some of the dependencies, by building binaries.
# Then remove these binaries and source code, so they're not used during CI jobs.
RUN git clone https://github.com/NVIDIA/aistore.git && cd aistore && \
    make && AIS_CLD_PROVIDERS="ais aws gcp azure" make node && \
    make clean && cd .. && rm -rf aistore
