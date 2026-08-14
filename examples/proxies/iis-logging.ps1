# Run elevated, replacing "Default Web Site" as needed. W3C writes selected
# fields in flag order, matching the iis-w3c decoder. UriQuery is omitted.
Import-Module WebAdministration
Set-ItemProperty 'IIS:\Sites\Default Web Site' -Name logFile.logFormat -Value W3C
Set-ItemProperty 'IIS:\Sites\Default Web Site' -Name logFile.logExtFileFlags -Value 'Date,Time,Method,UriStem,HttpStatus,BytesSent,TimeTaken,ProtocolVersion'
