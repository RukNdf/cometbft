logFolder="batchLogs/"
reportFile="base.md"
networkTemplate="networks/template.toml"
imgFolderName="reportImages/"

# Add full path
reportFile=$logFolder$reportFile

# Create image path
imgFolder=$logFolder$imgFolderName
mkdir -p $imgFolder

# Initialize markdown report
echo Generating report...
echo "# Report" > $reportFile

# Get experiment values
cd $logFolder
# Get all directories, sort them numerically (e.g. 1/ 10/ 2/ ->> 1/ 2/ 10/), and then remove the forward slash from the directory name
values=$(ls -d */ | sort -n | sed -e 's#/##' -e "s#reportImages##")
cd ..

# Use empty echo for new lines as "echo \n" isn't guaranteed to work across distros
echo "Template used:" >> $reportFile && echo "" >> $reportFile && echo "\`\`\`" >> $reportFile
cat $networkTemplate >> $reportFile
echo "\`\`\`" >> $reportFile && echo "" >> $reportFile
echo "---" >> $reportFile

echo "Tested values:" >> $reportFile && echo "" >> $reportFile
echo $values >> $reportFile && echo "" >> $reportFile

echo Copying graphs...

echo "## Latency histogram" >> $reportFile
for v in $values; do
	# Check if image exists
	if [ -f ${logFolder}${v}/latency_hist_cdf.png ]; then
		# Copy file to folder and add copy to report
		cp "${logFolder}${v}/latency_hist_cdf.png" "${imgFolder}${v}-latency_hist_cdf.png"
		echo "![](${imgFolderName}${v}-latency_hist_cdf.png \"${v}\")" >> $reportFile && echo "" >> $reportFile
		echo "${v}" >> $reportFile && echo "" >> $reportFile
	fi
done
echo "---" >> $reportFile && echo "" >> $reportFile

echo "## Tx per block" >> $reportFile
for v in $values; do
	# Check if image exists
	if [ -f ${logFolder}${v}/transacoes_hist_cdf.png ]; then
		# Copy file to folder and add copy to report
		cp "${logFolder}${v}/transacoes_hist_cdf.png" "${imgFolder}${v}-transacoes_hist_cdf.png"
		echo "![](${imgFolderName}${v}-transacoes_hist_cdf.png \"${v}\")" >> $reportFile && echo "" >> $reportFile
		echo "${v}" >> $reportFile && echo "" >> $reportFile
	fi
done
echo "---" >> $reportFile && echo "" >> $reportFile

echo "## Blocksize per time" >> $reportFile
for v in $values; do
	# Check if image exists
	if [ -f ${logFolder}${v}/transacoes_line.png ]; then
		# Copy file to folder and add copy to report
		cp "${logFolder}${v}/transacoes_line.png" "${imgFolder}${v}-transacoes_line.png"
		echo "![](${imgFolderName}${v}-transacoes_line.png \"${v}\")" >> $reportFile && echo "" >> $reportFile
		echo "${v}" >> $reportFile && echo "" >> $reportFile
	fi
done
echo "---" >> $reportFile && echo "" >> $reportFile

echo Done
