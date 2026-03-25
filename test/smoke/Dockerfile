FROM homebrew/brew

# Install spire (pulls in dolt + beads automatically)
RUN brew tap awell-health/tap && brew install awell-health/tap/spire

# Copy test script (as root, then fix ownership)
USER root
COPY smoke-test.sh /home/linuxbrew/smoke-test.sh
RUN chmod +x /home/linuxbrew/smoke-test.sh && chown linuxbrew:linuxbrew /home/linuxbrew/smoke-test.sh
USER linuxbrew

ENTRYPOINT ["/home/linuxbrew/smoke-test.sh"]
